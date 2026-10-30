// N6K_LISTEN mode: n6kd listens on a Unix socket and the parent dials once per
// client, relaying it in the same stream framing the extension reads from the
// socketpair, so each accepted fd is served by n6k_serve_fd as-is with no pump. The
// parent authenticates its client; the HELLO token is not checked here.

package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/duckdb/duckdb-go/v2"
	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/engine"
	"github.com/n6k-io/n6kd/internal/sqlquote"
)

// ServeSocket serves the client on fd until it leaves. The caller owns fd.
func ServeSocket(ctx context.Context, fd int, eng *engine.Engine, opts ServeOptions, log *zap.SugaredLogger) (err error) {
	session, err := eng.OpenSession(ctx, log)
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	// A CALL that never unwinds still owns the database and the connection.
	reactorStillHolds := false
	defer func() {
		if reactorStillHolds {
			return
		}
		err = errors.Join(err, session.Close())
	}()

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

	if err := eng.Admit(ctx, session, nil, log); err != nil {
		return err
	}
	id, err := createServeSession(ctx, conn, fd)
	if err != nil {
		return err
	}
	serveDone := make(chan error, 1)
	go func() {
		log.Debugf("handing fd %d to the reactor as session %s", fd, id.String())
		_, err := conn.ExecContext(context.WithoutCancel(ctx), serveFDStatement(id, eng.Catalogs(), opts))
		log.Debugf("the reactor returned the fd (err: %v)", err)
		serveDone <- err
	}()

	select {
	case err := <-serveDone:
		return err
	case <-ctx.Done():
	}
	// EOFs the reactor's dup of fd, which returns the CALL.
	if err := syscall.Shutdown(fd, syscall.SHUT_RDWR); err != nil {
		log.Debugf("shutting down the socket failed: %v", err)
	}
	select {
	case err := <-serveDone:
		return err
	case <-time.After(serveJoinTimeout):
		reactorStillHolds = true
		log.Errorf("the reactor still holds this session's database after %v; leaking it rather than closing underneath the call",
			serveJoinTimeout)
		return errors.New("serving call did not unwind")
	}
}

// How long the serving CALL gets to unwind after the socket drops. Above the
// reactor's own 5s drain of in-flight queries, which this has to outlast.
const serveJoinTimeout = 15 * time.Second

// The extension reads HELLO itself and, with no authenticate callback, acks any
// token.
func serveFDStatement(id duckdb.UUID, catalogs []string, opts ServeOptions) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CALL n6k_serve_fd(%s::UUID", sqlquote.String(id.String()))
	for _, c := range catalogs {
		fmt.Fprintf(&b, ", %s", sqlquote.String(c))
	}
	b.WriteString(opts.params())
	b.WriteString(")")
	return b.String()
}

const ListenEnv = "N6K_LISTEN"

// ListenSocket claims path, unlinking a stale socket file but refusing one a
// live server still accepts on. Closing the listener unlinks path.
func ListenSocket(path string) (*net.UnixListener, error) {
	if probe, err := net.Dial("unix", path); err == nil {
		return nil, errors.Join(fmt.Errorf("%s %s is in use by another server", ListenEnv, path), probe.Close())
	} else if errors.Is(err, syscall.ECONNREFUSED) {
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
		}
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen %s %s: %w", ListenEnv, path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, errors.Join(fmt.Errorf("chmod %s: %w", path, err), ln.Close())
	}
	return ln, nil
}

// ServeListen serves every client ln accepts, each in its own session, until
// ctx is done. A client that fails, or leaves before HELLO, is logged.
func ServeListen(ctx context.Context, ln *net.UnixListener, eng *engine.Engine, opts ServeOptions, log *zap.SugaredLogger) error {
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()

	var wg sync.WaitGroup
	defer wg.Wait()
	var active atomic.Int64
	for {
		conn, err := ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errors.Join(fmt.Errorf("accept: %w", err), ln.Close())
		}
		if opts.full(active.Add(1)) {
			active.Add(-1)
			log.Infof("socket client refused: %d connected is the cap", opts.MaxConnections)
			if err := conn.Close(); err != nil {
				log.Debugf("closing the refused connection failed: %v", err)
			}
			continue
		}
		// File dups conn; Fd puts the dup in blocking mode, which the reactor needs.
		f, err := conn.File()
		closeErr := conn.Close()
		if err != nil || closeErr != nil {
			log.Errorf("taking the accepted connection's fd failed: %v", errors.Join(err, closeErr))
			active.Add(-1)
			continue
		}
		wg.Go(func() {
			defer active.Add(-1)
			defer func() { _ = f.Close() }()
			if err := ServeSocket(ctx, int(f.Fd()), eng, opts, log); err != nil {
				log.Infof("socket client ended: %v", err)
			}
		})
	}
}
