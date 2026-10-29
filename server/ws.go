// The WebSocket endpoint. This file never decodes an n6k frame, and must not
// start: the protocol lives in the n6k_server extension, which is handed the
// socket via CALL n6k_serve_fd and reads it itself. Everything here is transport
// — accept the upgrade, build the session, shuttle bytes, tear down in order.
//
// The mechanism is a socketpair. We keep one end; the *number* of the other is
// passed into SQL, which the extension dup()s and then treats as its client
// connection. On the wire a WebSocket message maps to one stream-framed record
// (see pump.go), in both directions.

package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/engine"
	"github.com/n6k-io/n6kd/internal/sqlquote"
	"github.com/n6k-io/n6kd/internal/streamframe"
	"github.com/n6k-io/n6kd/internal/wstext"
)

// How long the serving CALL gets to unwind after the socket drops. Above the
// reactor's own 5s drain of in-flight queries, which this has to outlast.
const serveJoinTimeout = 15 * time.Second

// Browsers never apply CORS to a WebSocket handshake, so the origin list has to
// be enforced here. A request without an Origin header is not a browser and is
// let through; the token is what authenticates it.
func newUpgrader(origins []string) *websocket.Upgrader {
	any := slices.Contains(origins, "*")
	return &websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			return origin == "" || any || slices.Contains(origins, origin)
		},
	}
}

var activeSessions atomic.Int64

func serveWS(w http.ResponseWriter, r *http.Request, upgrader *websocket.Upgrader, eng *engine.Engine, mcpServer *mcp.Server, log *zap.SugaredLogger) {
	client := r.RemoteAddr
	if id := middleware.GetReqID(r.Context()); len(id) >= 6 {
		client = fmt.Sprintf("%s [%s]", r.RemoteAddr, id[len(id)-6:])
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written its own error response.
		log.Errorf("client %s failed to upgrade to a websocket: %v", client, err)
		return
	}
	ws.SetReadLimit(streamframe.MaxBytes)
	closer := &wsCloser{ws: ws, log: log.Named(client)}
	defer func() { closer.finish(err) }()

	active := activeSessions.Add(1)
	defer activeSessions.Add(-1)
	log.Infof("client %s connected, serving %s — %d now connected",
		client, strings.Join(eng.Catalogs(), ", "), active)

	var counts counters
	started := time.Now()
	err = serveSession(r.Context(), closer, eng, mcpServer, log.Named(client), &counts)
	elapsed := time.Since(started).Round(time.Millisecond)
	if errors.Is(err, engine.ErrUnauthorized) {
		log.Infof("client %s rejected: %v — %d still connected", client, err, activeSessions.Load()-1)
		return
	}
	if err != nil {
		log.Errorf("client %s failed after %v: %v — %d still connected",
			client, elapsed, err, activeSessions.Load()-1)
		return
	}
	log.Infof("client %s disconnected after %v, %s in / %s out — %d still connected",
		client, elapsed,
		traffic(counts.framesIn.Load(), counts.bytesIn.Load()),
		traffic(counts.framesOut.Load(), counts.bytesOut.Load()),
		activeSessions.Load()-1)
}

func traffic(frames, bytes int64) string {
	switch {
	case bytes >= 1<<20:
		return fmt.Sprintf("%d frames/%.1f MB", frames, float64(bytes)/(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%d frames/%.1f kB", frames, float64(bytes)/(1<<10))
	}
	return fmt.Sprintf("%d frames/%d B", frames, bytes)
}

// counters is what the session report is built from: written by the two pump
// goroutines. When the serving CALL ends first, the pumps may still be running
// as it is read, so the report can undercount.
type counters struct {
	framesIn  atomic.Int64
	bytesIn   atomic.Int64
	framesOut atomic.Int64
	bytesOut  atomic.Int64
}

// serveSession owns one client for its whole life: its session, a socketpair,
// and the blocking CALL that serves it.
func serveSession(ctx context.Context, closer *wsCloser, eng *engine.Engine, mcpServer *mcp.Server, log *zap.SugaredLogger, counts *counters) (err error) {
	ws := closer.ws
	text := wstext.New(ws)
	defer func() { _ = text.Close() }()
	session, err := eng.OpenSession(ctx, log)
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	// Set when the serving CALL never returns: a thread still inside the reactor
	// owns this database and this connection, so both closes are skipped rather
	// than raced against it. Leaking them is the lesser failure.
	reactorStillHolds := false
	// Closed last, and only after the serving CALL has unwound.
	defer func() {
		if reactorStillHolds {
			return
		}
		err = errors.Join(err, session.Close())
	}()

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("socketpair: %w", err)
	}
	// Our end, non-blocking or os.NewFile keeps it off Go's poller — and off the
	// poller Close defers close(2) until the parked pump read returns, which is
	// itself waiting on the EOF that close was meant to send.
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		return errors.Join(fmt.Errorf("set the host socket non-blocking: %w", err),
			syscall.Close(fds[0]), syscall.Close(fds[1]))
	}
	host := os.NewFile(uintptr(fds[0]), "n6k-host")
	// The extension's end stays a bare descriptor, and blocking: the reactor reads
	// it with plain read(2), retrying EINTR but not EAGAIN.
	extFD := fds[1]
	// Held open for the whole session, not closed right after the CALL is issued:
	// the extension does not dup() until Adopt runs on a worker thread.
	defer func() { err = errors.Join(err, syscall.Close(extFD)) }()

	// A dedicated connection, because the CALL blocks it until the client leaves.
	conn, err := session.DB().Conn(ctx)
	if err != nil {
		return fmt.Errorf("take connection: %w", err)
	}
	defer func() {
		if reactorStillHolds {
			return
		}
		err = errors.Join(err, conn.Close())
	}()

	var mcpGranted *atomic.Bool
	if mcpServer != nil {
		mcpSession, err := mcpServer.Connect(ctx, text, nil)
		if err != nil {
			return fmt.Errorf("start mcp session: %w", err)
		}
		defer func() { _ = mcpSession.Close() }()
		mcpGranted = new(atomic.Bool)
	}

	serveDone := make(chan error, 1)
	go func() { serveDone <- admitAndServe(ctx, conn, eng, session, extFD, text, mcpGranted, log) }()

	pumpErr, served := pumpUntilEitherEnds(ws, text, mcpGranted, host, serveDone, counts)
	_ = text.Close()

	// Closing our end EOFs the extension's, which ends its reader loop and returns
	// the CALL the select below waits on.
	if closeErr := host.Close(); closeErr != nil && pumpErr == nil {
		pumpErr = closeErr
	}
	// Unblocks a ws read still parked in the pump goroutine.
	closer.finish(pumpErr)

	if served {
		return pumpErr
	}
	select {
	case callErr := <-serveDone:
		if callErr != nil && pumpErr == nil {
			pumpErr = callErr
		}
	case <-time.After(serveJoinTimeout):
		reactorStillHolds = true
		log.Errorf("the reactor still holds this session's database after %v; leaking it rather than closing underneath the call",
			serveJoinTimeout)
		return errors.Join(pumpErr, errors.New("serving call did not unwind"))
	}
	return pumpErr
}

// The CALL is watched alongside the two directions so a rejected fd ends the
// connection with that error instead of leaving both sides blocked on a peer
// that will never speak.
func pumpUntilEitherEnds(ws *websocket.Conn, text *wstext.Conn, mcpGranted *atomic.Bool, host *os.File, serveDone <-chan error, counts *counters) (err error, served bool) {
	toSock := make(chan error, 1)
	go func() { toSock <- wsToSock(ws, text, mcpGranted, host, counts) }()

	toWS := make(chan error, 1)
	go func() { toWS <- sockToWS(text, host, counts) }()

	select {
	case err := <-toSock:
		return err, false
	case err := <-toWS:
		return err, false
	case err := <-serveDone:
		if errors.Is(err, engine.ErrUnauthorized) {
			return err, true
		}
		if err != nil {
			return fmt.Errorf("serve: %w", err), true
		}
		return nil, true
	}
}

type wsCloser struct {
	ws   *websocket.Conn
	log  *zap.SugaredLogger
	once sync.Once
}

func (c *wsCloser) finish(err error) {
	c.once.Do(func() {
		code, reason := closeFrame(err)
		closeWith(c.ws, code, reason, c.log)
		if closeErr := c.ws.Close(); closeErr != nil {
			c.log.Debugf("closing the websocket failed: %v", closeErr)
		}
	})
}

func closeFrame(err error) (int, string) {
	switch {
	case errors.Is(err, engine.ErrUnauthorized):
		return websocket.ClosePolicyViolation, err.Error()
	case err != nil:
		return websocket.CloseInternalServerErr, err.Error()
	}
	return websocket.CloseNormalClosure, ""
}

// mcpGranted is nil when this server has no MCP; otherwise it is set once the
// client is admitted and offered `mcp`, and until then text frames are dropped.
func admitAndServe(ctx context.Context, conn *sql.Conn, eng *engine.Engine, session *engine.Session, fd int, text *wstext.Conn, mcpGranted *atomic.Bool, log *zap.SugaredLogger) error {
	blocking := context.WithoutCancel(ctx)

	var token string
	var offersMCP bool
	if err := conn.QueryRowContext(blocking,
		fmt.Sprintf("SELECT token, list_contains(capabilities, 'mcp') FROM n6k_handshake(%d)", fd)).Scan(&token, &offersMCP); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	claims, err := eng.Authenticate(ctx, token)
	if err != nil {
		log.Debugf("token refused: %v", err)
		var frame []byte
		if rejectErr := conn.QueryRowContext(blocking,
			"SELECT frame FROM n6k_reject(?)", engine.ErrUnauthorized.Error()).Scan(&frame); rejectErr != nil {
			return errors.Join(engine.ErrUnauthorized, fmt.Errorf("reject: %w", rejectErr))
		}
		if sendErr := text.WriteMessage(websocket.BinaryMessage, frame); sendErr != nil {
			log.Debugf("sending HELLO_ERR failed: %v", sendErr)
		}
		return engine.ErrUnauthorized
	}

	grantMCP := mcpGranted != nil && offersMCP
	return admitAndCall(ctx, conn, eng, session, fd, claims, grantMCP, func() { mcpGranted.Store(true) }, log)
}

func admitAndCall(ctx context.Context, conn *sql.Conn, eng *engine.Engine, session *engine.Session, fd int,
	claims map[string]any, grantMCP bool, onGranted func(), log *zap.SugaredLogger,
) error {
	if err := eng.Admit(ctx, session, claims, log); err != nil {
		return err
	}
	if grantMCP {
		onGranted()
	}
	log.Debugf("handing fd %d to the reactor", fd)
	_, err := conn.ExecContext(context.WithoutCancel(ctx), serveStatement(fd, eng.Catalogs(), grantMCP))
	log.Debugf("the reactor returned the fd (err: %v)", err)
	return err
}

func serveStatement(fd int, catalogs []string, grantMCP bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CALL n6k_serve_fd(%d", fd)
	for _, c := range catalogs {
		fmt.Fprintf(&b, ", %s", sqlquote.String(c))
	}
	if grantMCP {
		b.WriteString(", capabilities => ['mcp'], app_capabilities => ['mcp']")
	}
	b.WriteString(")")
	return b.String()
}

// closeWith sends a close frame, truncated to the 123 bytes RFC 6455 allows.
func closeWith(ws *websocket.Conn, code int, reason string, log *zap.SugaredLogger) {
	if len(reason) > 123 {
		cut := 123
		for cut > 0 && !utf8.RuneStart(reason[cut]) {
			cut--
		}
		reason = reason[:cut]
	}
	deadline := time.Now().Add(time.Second)
	msg := websocket.FormatCloseMessage(code, reason)
	if err := ws.WriteControl(websocket.CloseMessage, msg, deadline); err != nil {
		log.Debugf("writing the close frame failed: %v", err)
	}
}
