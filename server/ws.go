// The WebSocket endpoint. This file never decodes an n6k frame, and must not
// start: the protocol lives in the n6k_server extension. The handler checks the
// origin, hijacks the connection once the upgrade request is read, and hands its
// fd to a serve session and CALL n6k_serve_ws, which sends the 101 and owns the
// socket from then on.
// No frame passes through Go; the extension calls back into callbacks.go to
// authenticate the client and to deliver MCP text.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/engine"
	"github.com/n6k-io/n6kd/internal/sqlquote"
)

// Browsers never apply CORS to a WebSocket handshake, so the origin list has to
// be enforced here. A request without an Origin header is not a browser and is
// let through; the token is what authenticates it.
func newOriginCheck(origins []string) func(*http.Request) bool {
	any := slices.Contains(origins, "*")
	return func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		return origin == "" || any || slices.Contains(origins, origin)
	}
}

var activeSessions atomic.Int64

func serveWS(w http.ResponseWriter, r *http.Request, checkOrigin func(*http.Request) bool, eng *engine.Engine, mcpServer *mcp.Server, opts ServeOptions, log *zap.SugaredLogger) {
	client := r.RemoteAddr
	if id := middleware.GetReqID(r.Context()); len(id) >= 6 {
		client = fmt.Sprintf("%s [%s]", r.RemoteAddr, id[len(id)-6:])
	}
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "expected a websocket upgrade", http.StatusBadRequest)
		return
	}
	if !checkOrigin(r) {
		log.Infof("client %s refused: origin %q is not allowed", client, r.Header.Get("Origin"))
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	request, err := upgradeRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	active := activeSessions.Add(1)
	defer activeSessions.Add(-1)
	if opts.full(active) {
		log.Infof("client %s refused: %d connected is the cap", client, opts.MaxConnections)
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	fd, err := hijackFD(w)
	if err != nil {
		log.Errorf("client %s: taking over the connection failed: %v", client, err)
		return
	}
	log.Infof("client %s connected, serving %s — %d now connected",
		client, strings.Join(eng.Catalogs(), ", "), active)

	stats, rejected, err := serveSession(r.Context(), fd, request, eng, mcpServer, opts, log.Named(client))
	if err != nil {
		log.Errorf("client %s failed: %v — %d still connected", client, err, activeSessions.Load()-1)
		return
	}
	if rejected {
		log.Infof("client %s rejected: %v — %d still connected", client, engine.ErrUnauthorized, activeSessions.Load()-1)
		return
	}
	log.Infof("client %s disconnected after %v, %s in / %s out, %d requests — %d still connected",
		client, time.Duration(stats.durationMs)*time.Millisecond,
		traffic(stats.framesIn, stats.bytesIn), traffic(stats.framesOut, stats.bytesOut),
		stats.requests, activeSessions.Load()-1)
	log.Debugf("client %s authenticated in %v", client, time.Duration(stats.authenticateMs)*time.Millisecond)
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

// upgradeRequest is the request n6k_serve_ws completes the upgrade from. Go
// lifts Host out of the header map, so it goes back in.
func upgradeRequest(r *http.Request) (string, error) {
	headers := make(map[string]string, len(r.Header)+1)
	for name, values := range r.Header {
		headers[name] = strings.Join(values, ", ")
	}
	headers["Host"] = r.Host
	b, err := json.Marshal(map[string]any{
		"method":  r.Method,
		"uri":     r.RequestURI,
		"version": r.Proto,
		"headers": headers,
	})
	return string(b), err
}

// hijackFD takes the connection away from net/http and returns a descriptor the
// caller owns. File returns a dup the *os.File would close, so it is dup'd again
// and every Go copy closed.
func hijackFD(w http.ResponseWriter) (int, error) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return -1, errors.New("the response writer cannot be hijacked")
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return -1, err
	}
	defer func() { _ = conn.Close() }()
	if n := rw.Reader.Buffered(); n > 0 {
		return -1, fmt.Errorf("%d bytes arrived past the upgrade request", n)
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return -1, fmt.Errorf("a %T cannot be handed over", conn)
	}
	f, err := tcp.File()
	if err != nil {
		return -1, err
	}
	defer func() { _ = f.Close() }()
	return syscall.Dup(int(f.Fd()))
}

type sessionStats struct {
	framesIn, bytesIn, framesOut, bytesOut, requests, durationMs, authenticateMs int64
}

// serveSession serves the client on fd until its WebSocket closes. The extension
// owns fd once the CALL binds; before that, fd is closed here.
func serveSession(ctx context.Context, fd int, request string, eng *engine.Engine, mcpServer *mcp.Server, opts ServeOptions, log *zap.SugaredLogger) (stats sessionStats, rejected bool, err error) {
	handedOver := false
	defer func() {
		if !handedOver {
			err = errors.Join(err, syscall.Close(fd))
		}
	}()

	session, err := eng.OpenSession(ctx, log)
	if err != nil {
		return stats, false, fmt.Errorf("open session: %w", err)
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	if err := registerCallbacks(ctx, session); err != nil {
		return stats, false, err
	}
	// A dedicated connection, because the CALL blocks it until the client leaves.
	conn, err := session.DB().Conn(ctx)
	if err != nil {
		return stats, false, fmt.Errorf("take connection: %w", err)
	}
	defer func() { err = errors.Join(err, conn.Close()) }()

	id, err := createServeSession(ctx, conn, fd)
	if err != nil {
		return stats, false, err
	}
	h := &host{ctx: ctx, id: id, eng: eng, session: session, mcpServer: mcpServer, log: log}
	hosts.Store(h.id, h)
	defer hosts.Delete(h.id)
	defer h.close()

	callbacks := "authenticate => '" + authenticateUDF + "'"
	if mcpServer != nil {
		callbacks += ", on_text => '" + onTextUDF + "'"
	}
	// #nosec G201 -- the statement holds only a session id, constant UDF names and integers.
	statement := fmt.Sprintf(`SELECT frames_in, bytes_in, frames_out, bytes_out, requests_handled, duration_ms, authenticate_ms
		FROM n6k_serve_ws(%s::UUID, ?, %s%s)`, sqlquote.String(id.String()), callbacks, opts.params())
	handedOver = true
	err = conn.QueryRowContext(context.WithoutCancel(ctx), statement, request).Scan(
		&stats.framesIn, &stats.bytesIn, &stats.framesOut, &stats.bytesOut,
		&stats.requests, &stats.durationMs, &stats.authenticateMs)
	h.mu.Lock()
	rejected = h.rejected
	h.mu.Unlock()
	return stats, rejected, err
}
