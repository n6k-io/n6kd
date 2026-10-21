// N6K_LISTEN mode: n6kd listens on a Unix socket and the parent dials once per
// client, relaying it in the same stream framing the extension reads from the
// socketpair, so each accepted fd goes to n6k_serve_fd as-is with no pump. The
// parent authenticates its client; the HELLO token is not checked here.

package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/engine"
)

// ServeSocket serves the client on fd until it leaves. The caller owns fd.
func ServeSocket(ctx context.Context, fd int, eng *engine.Engine, log *zap.SugaredLogger) (err error) {
	session, err := eng.OpenSession(ctx, log)
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	// As in serveSession: a CALL that never unwinds still owns the database and
	// the connection.
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

	serveDone := make(chan error, 1)
	go func() {
		var token string
		if err := conn.QueryRowContext(context.WithoutCancel(ctx),
			fmt.Sprintf("SELECT token FROM n6k_handshake(%d)", fd)).Scan(&token); err != nil {
			serveDone <- fmt.Errorf("handshake: %w", err)
			return
		}
		serveDone <- admitAndCall(ctx, conn, eng, session, fd, nil, false, nil, log)
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
func ServeListen(ctx context.Context, ln *net.UnixListener, eng *engine.Engine, log *zap.SugaredLogger) error {
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()

	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errors.Join(fmt.Errorf("accept: %w", err), ln.Close())
		}
		// File dups conn; Fd puts the dup in blocking mode, which the reactor needs.
		f, err := conn.File()
		closeErr := conn.Close()
		if err != nil || closeErr != nil {
			log.Errorf("taking the accepted connection's fd failed: %v", errors.Join(err, closeErr))
			continue
		}
		wg.Go(func() {
			defer func() { _ = f.Close() }()
			if err := ServeSocket(ctx, int(f.Fd()), eng, log); err != nil {
				log.Infof("socket client ended: %v", err)
			}
		})
	}
}
