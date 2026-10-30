package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
)

// listenUnix stands in for a live server: a short path, under the AF_UNIX limit.
func listenUnix(t *testing.T) (path string, listener int) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "n6kd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path = filepath.Join(dir, "db.sock")

	listener, err = syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(listener) })
	if err := syscall.Bind(listener, &syscall.SockaddrUnix{Name: path}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(listener, 1); err != nil {
		t.Fatal(err)
	}
	return path, listener
}

// dialUnix returns a blocking fd off Go's poller, as the extension's wsFd needs.
func dialUnix(path string) (int, error) {
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return -1, err
	}
	if err := syscall.Connect(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		return -1, errors.Join(err, syscall.Close(fd))
	}
	return fd, nil
}

func attachOverFD(t *testing.T, fd int, token string) *sql.DB {
	t.Helper()
	db, err := sql.Open("duckdb", "?allow_unsigned_extensions=true")
	if err != nil {
		t.Fatal(err)
	}
	loadClientExtension(t, db)
	var connected bool
	if err := db.QueryRowContext(t.Context(),
		"SELECT connected FROM n6k_connect(wsFd => ?, token => ?)", fd, token).Scan(&connected); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := db.ExecContext(t.Context(),
		fmt.Sprintf("ATTACH '' AS remote (TYPE n6k, wsFd %d, CATALOG 'memory')", fd)); err != nil {
		t.Fatalf("attach: %v", err)
	}
	return db
}

// connectOverSocket dials path and runs the handshake, returning its error
// rather than failing the test.
func connectOverSocket(t *testing.T, path string) error {
	t.Helper()
	fd, err := dialUnix(path)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(fd) }()
	db, err := sql.Open("duckdb", "?allow_unsigned_extensions=true")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	loadClientExtension(t, db)
	var connected bool
	return db.QueryRowContext(t.Context(),
		"SELECT connected FROM n6k_connect(wsFd => ?, token => '')", fd).Scan(&connected)
}

func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "n6kd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "db.sock")
}

func queryUsersOverSocket(t *testing.T, path, token string) error {
	fd, err := dialUnix(path)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(fd) }()
	db := attachOverFD(t, fd, token)
	defer func() { _ = db.Close() }()
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM remote.main.users`).Scan(&count); err != nil {
		return fmt.Errorf("query over socket: %w", err)
	}
	if count != 2 {
		return fmt.Errorf("remote.main.users: got %d rows, want 2", count)
	}
	_, err = db.ExecContext(t.Context(), "DETACH remote")
	return err
}

// Mirrors the kernel relay: a probe connection closed before HELLO, then
// several clients at once.
func TestListenServesConcurrentClientsAfterAProbe(t *testing.T) {
	path := shortSocketPath(t)
	ln, err := ListenSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket file: %v, %v; want mode 0600", info, err)
	}

	eng := newDirectEngine(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- ServeListen(ctx, ln, eng, ServeOptions{}, zaptest.NewLogger(t).Sugar()) }()

	probe, err := dialUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Close(probe); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Go(func() { errs[i] = queryUsersOverSocket(t, path, "") })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}

	cancel()
	if err := <-served; err != nil {
		t.Fatalf("ServeListen: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket file still there after shutdown: %v", err)
	}
}

func TestListenIgnoresTheHelloToken(t *testing.T) {
	path := shortSocketPath(t)
	ln, err := ListenSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	eng := newDirectEngine(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- ServeListen(ctx, ln, eng, ServeOptions{}, zaptest.NewLogger(t).Sugar()) }()

	if err := queryUsersOverSocket(t, path, "any-token"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatalf("ServeListen: %v", err)
	}
}

func TestListenRefusesPastMaxConnections(t *testing.T) {
	path := shortSocketPath(t)
	ln, err := ListenSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	eng := newDirectEngine(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() {
		served <- ServeListen(ctx, ln, eng, ServeOptions{MaxConnections: 1}, zaptest.NewLogger(t).Sugar())
	}()

	held, err := dialUnix(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Close(held) }()
	db := attachOverFD(t, held, "")
	defer func() { _ = db.Close() }()

	if err := connectOverSocket(t, path); err == nil {
		t.Error("a second client connected past --max-connections 1")
	}

	if _, err := db.ExecContext(t.Context(), "DETACH remote"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatalf("ServeListen: %v", err)
	}
}

// An out-of-range value the extension refuses shows each option reaches its CALL.
func TestListenPassesServeOptionsToTheExtension(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opts   ServeOptions
		served bool
	}{
		{"in range", ServeOptions{ParallelRequests: 1, IdleStreamTimeout: time.Minute, IdleTransactionTimeout: time.Minute}, true},
		{"parallel requests out of range", ServeOptions{ParallelRequests: 1 << 20}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := shortSocketPath(t)
			ln, err := ListenSocket(path)
			if err != nil {
				t.Fatal(err)
			}
			eng := newDirectEngine(t, "")
			ctx, cancel := context.WithCancel(t.Context())
			served := make(chan error, 1)
			go func() { served <- ServeListen(ctx, ln, eng, tc.opts, zaptest.NewLogger(t).Sugar()) }()

			if tc.served {
				if err := queryUsersOverSocket(t, path, ""); err != nil {
					t.Errorf("not served with %+v: %v", tc.opts, err)
				}
			} else if err := connectOverSocket(t, path); err == nil {
				t.Errorf("served with %+v, which the extension should refuse", tc.opts)
			}
			cancel()
			if err := <-served; err != nil {
				t.Fatalf("ServeListen: %v", err)
			}
		})
	}
}

func TestListenSocketReclaimsAStaleFile(t *testing.T) {
	path := shortSocketPath(t)
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Close(fd); err != nil {
		t.Fatal(err)
	}

	ln, err := ListenSocket(path)
	if err != nil {
		t.Fatalf("stale socket file not reclaimed: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestListenSocketRefusesALiveServer(t *testing.T) {
	path, _ := listenUnix(t)
	if _, err := ListenSocket(path); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("got %v, want in use", err)
	}
}
