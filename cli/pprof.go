package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
	"time"

	"go.uber.org/zap"
)

// startPprof serves net/http/pprof on its own listener, apart from the clients'
// port, until ctx ends. Block and mutex profiles are sampled only while it runs.
func startPprof(ctx context.Context, addr string, log *zap.SugaredLogger) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	runtime.SetBlockProfileRate(int(10 * time.Microsecond))
	runtime.SetMutexProfileFraction(10)

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("pprof: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	log.Infof("pprof at http://%s/debug/pprof/", listener.Addr())
	return nil
}
