package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"runtime/debug"
	"slices"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/engine"
	"github.com/n6k-io/n6kd/internal/sqlquote"
)

func newMCPCmd() *cobra.Command {
	var configPath string
	var debugLogs, jsonLogs bool

	cmd := &cobra.Command{
		Use:   "mcp [--config ~/.n6kd/mcp.yml]",
		Short: "Serve MCP over stdio from a scratch DuckDB connected to n6k servers",
		Long: "Opens an in-memory DuckDB with n6k_client loaded, holds one WebSocket to each\n" +
			"server in --config, and speaks MCP on stdin/stdout. Servers can also be added and\n" +
			"removed with the add_server and remove_server tools. Each server's token is resolved\n" +
			"by n6k_connect from DuckDB secrets scoped to its host:port: the API key n6kd auth login\n" +
			"--server stores, or the n6k_refresh secret n6kd auth login --issuer stores.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			cmd.SilenceUsage = true

			log, err := installLogger(debugLogs, jsonLogs)
			if err != nil {
				return err
			}
			defer func() { _ = log.Sync() }()

			if configPath == "" {
				return errors.New("--config is required when the home directory is unknown")
			}
			return runMCP(cmd.Context(), configPath, &mcp.StdioTransport{}, log)
		},
	}

	cmd.Flags().StringVar(&configPath, "config", defaultMCPConfigPath(), "servers to connect at startup")
	cmd.Flags().BoolVar(&debugLogs, "debug", os.Getenv("N6KD_DEBUG") != "", "log connect, attach and query detail")
	cmd.Flags().BoolVar(&jsonLogs, "json-logging", os.Getenv("N6KD_JSON_LOGGING") != "",
		"log JSON lines with Cloud Logging severity fields, for log collectors")
	return cmd
}

func runMCP(ctx context.Context, configPath string, transport mcp.Transport, log *zap.SugaredLogger) (err error) {
	config, err := loadMCPConfig(configPath)
	if err != nil {
		return err
	}
	db, err := engine.OpenClient(ctx, log)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()

	server := mcp.NewServer(&mcp.Implementation{Name: "n6kd", Version: buildVersion()}, nil)
	session := &mcpSession{conn: conn, server: server, configPath: configPath, log: log, servers: map[string]*serverConn{}}
	session.register(server)
	defer func() { err = errors.Join(err, session.disconnectAll(context.WithoutCancel(ctx))) }()

	for _, name := range slices.Sorted(maps.Keys(config.Servers)) {
		sc := newServerConn(name, config.Servers[name].URL)
		session.servers[name] = sc
		if err := session.connect(ctx, sc); err != nil {
			log.Warnf("%s: connect deferred to its next use: %v", name, err)
		}
	}
	return server.Run(ctx, transport)
}

type mcpSession struct {
	mu         sync.Mutex
	conn       *sql.Conn
	server     *mcp.Server
	configPath string
	log        *zap.SugaredLogger
	servers    map[string]*serverConn
}

type serverConn struct {
	name, url   string
	link        *wsLink
	remote      *mcp.ClientSession
	remoteTools []string
	attached    map[string]string
}

func newServerConn(name, url string) *serverConn {
	return &serverConn{name: name, url: url, attached: map[string]string{}}
}

func (s *mcpSession) lookup(name string) (*serverConn, error) {
	sc, ok := s.servers[name]
	if !ok {
		return nil, fmt.Errorf("no server named %q", name)
	}
	return sc, nil
}

func (s *mcpSession) connect(ctx context.Context, sc *serverConn) error {
	if sc.link != nil && sc.link.alive() {
		return nil
	}
	if sc.link != nil {
		s.log.Warnf("%s: connection to %s dropped (%v); reconnecting", sc.name, sc.url, sc.link.err)
		if err := s.disconnect(ctx, sc); err != nil {
			s.log.Warnf("%s: tearing down the dropped connection: %v", sc.name, err)
		}
	}

	scope, err := serverScope(sc.url)
	if err != nil {
		return err
	}
	link, err := dialLink(ctx, sc.url)
	if err != nil {
		return err
	}
	var connected, servesMCP bool
	if err := s.conn.QueryRowContext(ctx,
		"SELECT connected, list_contains(capabilities, 'mcp') FROM n6k_connect(wsFd => ?, scope => ?)",
		link.extFD, scope).Scan(&connected, &servesMCP); err != nil {
		return errors.Join(fmt.Errorf("connect to %s: %w", sc.url, err), link.close(true))
	}
	sc.link = link
	s.log.Infof("%s: connected to %s", sc.name, sc.url)

	var errs []error
	for alias, catalog := range sc.attached {
		if _, err := s.attachOverLink(ctx, sc, alias, catalog); err != nil {
			delete(sc.attached, alias)
			errs = append(errs, err)
		}
	}
	if servesMCP {
		if err := s.connectRemote(ctx, sc); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *mcpSession) disconnect(ctx context.Context, sc *serverConn) error {
	if sc.link == nil {
		return nil
	}
	remoteErr := s.disconnectRemote(sc)
	detached := true
	for alias := range sc.attached {
		if _, err := s.conn.ExecContext(ctx, "DETACH DATABASE IF EXISTS "+sqlquote.Ident(alias)); err != nil {
			s.log.Warnf("%s: detach %s: %v; keeping fd %d open so its number is not reused", sc.name, alias, err, sc.link.extFD)
			detached = false
		}
	}
	err := sc.link.close(detached)
	sc.link = nil
	return errors.Join(remoteErr, err)
}

func (s *mcpSession) disconnectAll(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for _, sc := range s.servers {
		errs = append(errs, s.disconnect(ctx, sc))
	}
	return errors.Join(errs...)
}

func (s *mcpSession) attachOverLink(ctx context.Context, sc *serverConn, alias, catalog string) (string, error) {
	stmt := fmt.Sprintf("ATTACH '' AS %s (TYPE n6k, wsFd %d, CATALOG %s)", sqlquote.Ident(alias), sc.link.extFD, sqlquote.String(catalog))
	if _, err := s.conn.ExecContext(ctx, stmt); err != nil {
		return stmt, fmt.Errorf("`%s` failed: %w", stmt, err)
	}
	s.log.Infof("%s: attached catalog %q as %q", sc.name, catalog, alias)
	return stmt, nil
}

func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "dev"
}
