// The WebSocket endpoint. This file never decodes an n6k frame, and must not
// start: the protocol lives in the n6k_server extension, which is handed the
// socket via CALL n6k_serve_fd and reads it itself. Everything here is transport
// — accept the upgrade, build the session, shuttle bytes, tear down in order.
//
// The mechanism is a socketpair. We keep one end; the *number* of the other is
// passed into SQL, which the extension dup()s and then treats as its client
// connection. On the wire a WebSocket binary message maps to one length-prefixed
// record, 4-byte big-endian, in both directions.

package server

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/engine"
)

// Bounds a corrupt length prefix from the extension; far above any real frame.
const maxFrameBytes = 1 << 30

// How long the serving CALL gets to unwind after the socket drops. Above the
// reactor's own 5s drain of in-flight queries, which this has to outlast.
const serveJoinTimeout = 15 * time.Second

// Browsers never apply CORS to a WebSocket handshake, so the origin list has to
// be enforced here. A request without an Origin header is not a browser and is
// let through; the API key is what authenticates it.
func newUpgrader(origins []string) *websocket.Upgrader {
	any := slices.Contains(origins, "*")
	return &websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			return origin == "" || any || slices.Contains(origins, origin)
		},
	}
}

// Live sessions, reported in the connect and disconnect lines.
var activeSessions atomic.Int64

func serveWS(w http.ResponseWriter, r *http.Request, upgrader *websocket.Upgrader, eng *engine.Engine, log *zap.SugaredLogger) {
	// The last 6 of chi's request id, to tell concurrent clients apart in the log.
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
	defer func() {
		if err := ws.Close(); err != nil {
			log.Debugf("client %s: closing the websocket failed: %v", client, err)
		}
	}()

	ws.SetReadLimit(maxFrameBytes)

	active := activeSessions.Add(1)
	defer activeSessions.Add(-1)
	log.Infof("client %s connected, serving %s — %d now connected",
		client, strings.Join(eng.Catalogs(), ", "), active)

	var counts counters
	started := time.Now()
	// Named rather than prefixed by hand, so the session and engine lines below
	// carry the client without threading a string through every call.
	err = serveSession(r.Context(), ws, eng, log.Named(client), &counts)
	elapsed := time.Since(started).Round(time.Millisecond)
	if err != nil {
		log.Errorf("client %s failed after %v: %v — %d still connected",
			client, elapsed, err, activeSessions.Load()-1)
		// Tell the client why rather than dropping the stream, which is
		// indistinguishable from a network failure.
		closeWith(ws, websocket.CloseInternalServerErr, err.Error(), log)
		return
	}
	log.Infof("client %s disconnected after %v, %s in / %s out — %d still connected",
		client, elapsed,
		traffic(counts.framesIn.Load(), counts.bytesIn.Load()),
		traffic(counts.framesOut.Load(), counts.bytesOut.Load()),
		activeSessions.Load()-1)
	closeWith(ws, websocket.CloseNormalClosure, "", log)
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
// goroutines, read once both have stopped.
type counters struct {
	framesIn  atomic.Int64
	bytesIn   atomic.Int64
	framesOut atomic.Int64
	bytesOut  atomic.Int64
}

// serveSession owns one client for its whole life: a private bridged database,
// a socketpair, and the blocking CALL that serves it.
func serveSession(ctx context.Context, ws *websocket.Conn, eng *engine.Engine, log *zap.SugaredLogger, counts *counters) (err error) {
	session, err := eng.NewSession(ctx, log)
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

	serveDone := make(chan error, 1)
	go func() {
		// Deliberately not ctx: cancelling the request must not kill the CALL out
		// from under the reactor. Closing our socket end is what ends it, below.
		log.Debugf("handing fd %d to the reactor", extFD)
		_, callErr := conn.ExecContext(context.WithoutCancel(ctx),
			fmt.Sprintf("CALL n6k_serve_fd(%d)", extFD))
		log.Debugf("the reactor returned the fd (err: %v)", callErr)
		serveDone <- callErr
	}()

	pumpErr := pumpUntilEitherEnds(ws, host, serveDone, counts)

	// Closing our end EOFs the extension's, which ends its reader loop and returns
	// the CALL the select below waits on.
	if closeErr := host.Close(); closeErr != nil && pumpErr == nil {
		pumpErr = closeErr
	}
	// Unblocks a ws read still parked in the pump goroutine.
	if closeErr := ws.Close(); closeErr != nil {
		log.Debugf("closing the websocket during teardown failed: %v", closeErr)
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
func pumpUntilEitherEnds(ws *websocket.Conn, host *os.File, serveDone <-chan error, counts *counters) error {
	toSock := make(chan error, 1)
	go func() { toSock <- wsToSock(ws, host, counts) }()

	toWS := make(chan error, 1)
	go func() { toWS <- sockToWS(ws, host, counts) }()

	select {
	case err := <-toSock:
		return err
	case err := <-toWS:
		return err
	case err := <-serveDone:
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	}
}

func wsToSock(ws *websocket.Conn, host *os.File, counts *counters) error {
	var header [4]byte
	for {
		messageType, payload, err := ws.ReadMessage()
		if err != nil {
			// A client going away — cleanly, or by dropping the connection — is an
			// ordinary end of stream, and so is our own teardown closing this socket
			// out from under the read. Anything else is a real failure to report.
			if websocket.IsCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway,
				websocket.CloseNoStatusReceived,
				websocket.CloseAbnormalClosure,
			) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		// SetReadLimit already caps this; the check keeps the conversion below
		// provably in range rather than relying on a setting made elsewhere.
		if len(payload) > maxFrameBytes {
			return fmt.Errorf("client frame of %d bytes exceeds the %d byte limit", len(payload), maxFrameBytes)
		}
		// #nosec G115 -- the check above bounds this by maxFrameBytes (1<<30).
		binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
		if _, err := host.Write(header[:]); err != nil {
			return err
		}
		if _, err := host.Write(payload); err != nil {
			return err
		}
		counts.framesIn.Add(1)
		counts.bytesIn.Add(int64(len(payload)))
	}
}

func sockToWS(ws *websocket.Conn, host *os.File, counts *counters) error {
	reader := bufio.NewReader(host)
	var header [4]byte
	for {
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil // the extension closed
			}
			return err
		}
		size := binary.BigEndian.Uint32(header[:])
		if size > maxFrameBytes {
			return fmt.Errorf("frame of %d bytes exceeds the %d byte limit", size, maxFrameBytes)
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return err
		}
		if err := ws.WriteMessage(websocket.BinaryMessage, payload); err != nil {
			return err
		}
		counts.framesOut.Add(1)
		counts.bytesOut.Add(int64(size))
	}
}

// closeWith sends a close frame, truncated to the 123 bytes RFC 6455 allows.
func closeWith(ws *websocket.Conn, code int, reason string, log *zap.SugaredLogger) {
	if len(reason) > 123 {
		reason = reason[:123]
	}
	deadline := time.Now().Add(time.Second)
	msg := websocket.FormatCloseMessage(code, reason)
	if err := ws.WriteControl(websocket.CloseMessage, msg, deadline); err != nil {
		log.Debugf("writing the close frame failed: %v", err)
	}
}
