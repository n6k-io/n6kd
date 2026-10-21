package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/engine"
)

// How long in-flight requests get to finish after a shutdown signal.
const shutdownGrace = 10 * time.Second

type Config struct {
	Addr           string
	AllowedOrigins []string
	// MCP, when set, is served on each websocket's text frames to clients that offer `mcp`.
	MCP *mcp.Server
}

func Run(ctx context.Context, cfg Config, eng *engine.Engine, log *zap.SugaredLogger) error {
	srv := &http.Server{
		Handler:           newRouter(cfg, eng, log),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Bound here rather than inside ListenAndServe so a port already in use is
	// reported as such, and so the line below is only logged once it is true.
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	authority := dialableAuthority(listener.Addr())
	log.Info("ready — clients attach with:")
	for _, catalog := range eng.Catalogs() {
		log.Infof("  %s", attachStatement(authority, catalog))
	}
	log.Debugf("health check at http://%s/healthz", authority)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(listener) }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	log.Infof("shutting down, giving clients up to %v to finish", shutdownGrace)
	// Detached from ctx, which is already cancelled — this is the grace window.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Errorf("shutdown gave up after %v with clients still connected: %v", shutdownGrace, err)
		return err
	}
	log.Info("stopped")
	return nil
}

// attachStatement is what a client actually runs. The alias defaults to the
// server-side catalog name, so a name every DuckDB already has — "memory",
// which direct mode serves — must be given as the CATALOG option instead.
func attachStatement(authority, catalog string) string {
	if reservedNames[strings.ToLower(catalog)] {
		return fmt.Sprintf("ATTACH 'n6k://%s' AS remote (TYPE n6k, CATALOG '%s')", authority, catalog)
	}
	return fmt.Sprintf("ATTACH 'n6k://%s' AS %s (TYPE n6k)", authority, catalog)
}

var reservedNames = map[string]bool{"memory": true, "system": true, "temp": true}

// dialableAuthority turns the bound address into one a client can dial. Listening
// on every interface prints as ":8080" or "[::]:8080", neither of which can be
// pasted into an ATTACH.
func dialableAuthority(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}

func newRouter(cfg Config, eng *engine.Engine, log *zap.SugaredLogger) http.Handler {
	origins := cfg.AllowedOrigins
	if len(origins) == 0 {
		origins = []string{"*"}
	}
	upgrader := newUpgrader(origins)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger(log))
	r.Use(middleware.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   origins,
		AllowedMethods:   []string{"GET", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type"},
		AllowCredentials: !slices.Contains(origins, "*"),
		MaxAge:           300,
	}))

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	r.Get("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWS(w, r, upgrader, eng, cfg.MCP, log)
	})

	return r
}

// requestLogger logs every request at info, except /healthz and /ws, which are
// logged at debug. serveWS logs its own connect and disconnect lines.
func requestLogger(log *zap.SugaredLogger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			line := func(template string, args ...any) { log.Infof(template, args...) }
			if r.URL.Path == "/healthz" || r.URL.Path == "/ws" {
				line = func(template string, args ...any) { log.Debugf(template, args...) }
			}
			line("%s %s from %s → %d in %v",
				r.Method, r.URL.Path, r.RemoteAddr, ww.Status(),
				time.Since(started).Round(time.Millisecond))
		})
	}
}
