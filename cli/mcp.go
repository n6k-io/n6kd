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
	sendConn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, sendConn.Close()) }()

	server := mcp.NewServer(&mcp.Implementation{Name: "n6kd", Version: buildVersion()}, nil)
	session := &mcpSession{
		conn: conn, sendConn: sendConn, texts: newTextRoutes(),
		server: server, configPath: configPath, log: log, servers: map[string]*serverConn{},
	}
	if err := session.texts.register(conn); err != nil {
		return err
	}
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
	sendConn   *sql.Conn
	texts      *textRoutes
	server     *mcp.Server
	configPath string
	log        *zap.SugaredLogger
	servers    map[string]*serverConn
}

type serverConn struct {
	name, url   string
	id          string
	servesMCP   bool
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
	if sc.id != "" {
		alive, found, err := s.connState(ctx, sc.id)
		if err != nil {
			return err
		}
		switch {
		case !found:
			s.forgetConnection(sc)
		case !alive:
			s.log.Warnf("%s: connection to %s dropped; reconnecting", sc.name, sc.url)
			if err := s.disconnectRemote(sc); err != nil {
				s.log.Warnf("%s: closing the dropped mcp session: %v", sc.name, err)
			}
			// on_close runs on the extension's own thread: if it landed after the redial it would end the new session.
			s.texts.awaitClose(ctx, sc.id)
			if err := s.conn.QueryRowContext(ctx,
				"SELECT list_contains(capabilities, 'mcp') FROM n6k_reconnect(?)", sc.id).Scan(&sc.servesMCP); err != nil {
				return fmt.Errorf("reconnect to %s: %w", sc.url, err)
			}
			s.log.Infof("%s: reconnected to %s", sc.name, sc.url)
		}
	}
	if sc.id == "" {
		if err := s.openConnection(ctx, sc); err != nil {
			return err
		}
	}
	if !sc.servesMCP || s.texts.has(sc.id) {
		return nil
	}
	if err := s.disconnectRemote(sc); err != nil {
		s.log.Warnf("%s: closing the dropped mcp session: %v", sc.name, err)
	}
	return s.connectRemote(ctx, sc)
}

func (s *mcpSession) openConnection(ctx context.Context, sc *serverConn) error {
	target, err := extensionURL(sc.url)
	if err != nil {
		return err
	}
	if err := s.conn.QueryRowContext(ctx,
		"SELECT connection_id::VARCHAR, list_contains(capabilities, 'mcp') FROM n6k_connect(url => ?, on_text => '"+onTextUDF+"', on_close => '"+onCloseUDF+"')",
		target).Scan(&sc.id, &sc.servesMCP); err != nil {
		return fmt.Errorf("connect to %s: %w", sc.url, err)
	}
	s.log.Infof("%s: connected to %s", sc.name, sc.url)
	return nil
}

func (s *mcpSession) connState(ctx context.Context, id string) (alive, found bool, err error) {
	err = s.conn.QueryRowContext(ctx,
		"SELECT alive FROM n6k_connections() WHERE connection_id = ?::UUID", id).Scan(&alive)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return alive, true, nil
}

// A connection only leaves n6k_connections with its last lease, so no catalog is still attached over it.
func (s *mcpSession) forgetConnection(sc *serverConn) {
	if err := s.disconnectRemote(sc); err != nil {
		s.log.Warnf("%s: closing the mcp session: %v", sc.name, err)
	}
	s.texts.close(sc.id)
	sc.id = ""
	clear(sc.attached)
}

func (s *mcpSession) disconnect(ctx context.Context, sc *serverConn) error {
	if sc.id == "" {
		return nil
	}
	remoteErr := s.disconnectRemote(sc)
	for alias := range sc.attached {
		if _, err := s.conn.ExecContext(ctx, "DETACH DATABASE IF EXISTS "+sqlquote.Ident(alias)); err != nil {
			s.log.Warnf("%s: detach %s: %v; its lease keeps the connection open", sc.name, alias, err)
		}
	}
	s.texts.close(sc.id)
	_, err := s.conn.ExecContext(ctx, "SELECT * FROM n6k_disconnect(?)", sc.id)
	sc.id = ""
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

func (s *mcpSession) attachCatalog(ctx context.Context, sc *serverConn, alias, catalog string) (string, error) {
	stmt := fmt.Sprintf("ATTACH '' AS %s (TYPE n6k, CONNECTION %s, CATALOG %s)", sqlquote.Ident(alias), sqlquote.String(sc.id), sqlquote.String(catalog))
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
