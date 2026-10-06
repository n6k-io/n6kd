package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/engine"
)

type mcpFlags struct {
	server, apiKey string
}

func (f mcpFlags) scope() (string, error) {
	u, err := url.Parse(f.server)
	if err != nil {
		return "", fmt.Errorf("--server: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("--server %q has no host", f.server)
	}
	return u.Host, nil
}

func newMCPCmd() *cobra.Command {
	var f mcpFlags
	var debugLogs, jsonLogs bool

	cmd := &cobra.Command{
		Use:   "mcp --server ws://host:port/ws",
		Short: "Serve MCP over stdio from a scratch DuckDB connected to an n6kd server",
		Long: "Opens an in-memory DuckDB with n6k_client loaded, holds one WebSocket to the\n" +
			"server for the life of the process, and speaks MCP on stdin/stdout. With --api-key\n" +
			"that token is sent; otherwise n6k_connect resolves one from DuckDB secrets scoped to\n" +
			"the server's host:port, including the n6k_refresh secret n6kd auth login stores.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			cmd.SilenceUsage = true

			log, err := installLogger(debugLogs, jsonLogs)
			if err != nil {
				return err
			}
			defer func() { _ = log.Sync() }()

			if f.server == "" {
				return errors.New("--server is required")
			}
			if _, err := f.scope(); err != nil {
				return err
			}
			return runMCP(cmd.Context(), f, &mcp.StdioTransport{}, log)
		},
	}

	cmd.Flags().StringVar(&f.server, "server", os.Getenv("N6KD_SERVER"), "n6kd WebSocket endpoint, as ws://host:port/ws")
	cmd.Flags().StringVar(&f.apiKey, "api-key", os.Getenv("N6KD_API_KEY"), "token to present; the server's --api-key")
	cmd.Flags().BoolVar(&debugLogs, "debug", os.Getenv("N6KD_DEBUG") != "", "log connect, attach and query detail")
	cmd.Flags().BoolVar(&jsonLogs, "json-logging", os.Getenv("N6KD_JSON_LOGGING") != "",
		"log JSON lines with Cloud Logging severity fields, for log collectors")
	return cmd
}

func runMCP(ctx context.Context, f mcpFlags, transport mcp.Transport, log *zap.SugaredLogger) (err error) {
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

	session := &mcpSession{conn: conn, flags: f, log: log, attached: map[string]string{}}
	defer func() { err = errors.Join(err, session.disconnect(context.WithoutCancel(ctx))) }()
	if err := session.connect(ctx); err != nil {
		log.Warnf("connect deferred to the first tool call: %v", err)
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "n6kd", Version: buildVersion()}, nil)
	session.register(server)
	return server.Run(ctx, transport)
}

type mcpSession struct {
	mu       sync.Mutex
	conn     *sql.Conn
	flags    mcpFlags
	log      *zap.SugaredLogger
	link     *wsLink
	attached map[string]string
}

func (s *mcpSession) connect(ctx context.Context) error {
	if s.link != nil && s.link.alive() {
		return nil
	}
	if s.link != nil {
		s.log.Warnf("connection to %s dropped (%v); reconnecting", s.flags.server, s.link.err)
		if err := s.disconnect(ctx); err != nil {
			s.log.Warnf("tearing down the dropped connection: %v", err)
		}
	}

	scope, err := s.flags.scope()
	if err != nil {
		return err
	}
	link, err := dialLink(ctx, s.flags.server)
	if err != nil {
		return err
	}
	query, args := "SELECT connected FROM n6k_connect(wsFd => ?, scope => ?)", []any{link.extFD, scope}
	if s.flags.apiKey != "" {
		query, args = "SELECT connected FROM n6k_connect(wsFd => ?, token => ?)", []any{link.extFD, s.flags.apiKey}
	}
	var connected bool
	if err := s.conn.QueryRowContext(ctx, query, args...).Scan(&connected); err != nil {
		return errors.Join(fmt.Errorf("connect to %s: %w", s.flags.server, err), link.close(true))
	}
	s.link = link
	s.log.Infof("connected to %s", s.flags.server)

	var errs []error
	for alias, catalog := range s.attached {
		if _, err := s.attachOverLink(ctx, alias, catalog); err != nil {
			delete(s.attached, alias)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *mcpSession) disconnect(ctx context.Context) error {
	if s.link == nil {
		return nil
	}
	detached := true
	for alias := range s.attached {
		if _, err := s.conn.ExecContext(ctx, "DETACH DATABASE IF EXISTS "+sqlIdent(alias)); err != nil {
			s.log.Warnf("detach %s: %v; keeping fd %d open so its number is not reused", alias, err, s.link.extFD)
			detached = false
		}
	}
	err := s.link.close(detached)
	s.link = nil
	return err
}

func (s *mcpSession) attachOverLink(ctx context.Context, alias, catalog string) (string, error) {
	stmt := fmt.Sprintf("ATTACH '' AS %s (TYPE n6k, wsFd %d, CATALOG %s)", sqlIdent(alias), s.link.extFD, sqlString(catalog))
	if _, err := s.conn.ExecContext(ctx, stmt); err != nil {
		return stmt, fmt.Errorf("`%s` failed: %w", stmt, err)
	}
	s.log.Infof("attached catalog %q as %q", catalog, alias)
	return stmt, nil
}

func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func sqlIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "dev"
}
