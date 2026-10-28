package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/n6k-io/n6kd/internal/sqlquote"
	"github.com/n6k-io/n6kd/internal/textfmt"
)

// ServeQuack starts DuckDB's quack server on uri (quack:host:port) and returns
// the URL it listens on and the token clients must present: token, or a random
// one when token is empty. It returns once listening; Close stops it. Requires
// Options.Quack.
//
// With perms the served database is a vcatbridge: main database over the one
// catalog, since a quack client's ATTACH sees only the main database. Without,
// it is the source.
func (e *Engine) ServeQuack(ctx context.Context, uri, token string, allowOtherHost bool) (url, authToken string, err error) {
	if !e.opts.Quack {
		return "", "", errors.New("engine not opened for quack")
	}
	db := e.source
	if e.opts.Perms != nil {
		if db, err = e.openQuackBridge(ctx); err != nil {
			return "", "", err
		}
		e.quack = db
	}

	for _, name := range quackExtensions {
		if _, err := db.ExecContext(ctx, "LOAD "+name); err != nil {
			return "", "", fmt.Errorf("load %s: %w", name, err)
		}
	}

	// A token quack generates itself goes through the secret manager, after
	// which lockdown cannot change its settings.
	if token == "" {
		if token, err = randomHex(); err != nil {
			return "", "", err
		}
	}
	args := sqlquote.String(uri) + ", token := " + sqlquote.String(token)
	if allowOtherHost {
		args += ", allow_other_hostname := true"
	}
	// #nosec G201 -- args holds only values quoted by sqlquote.String.
	if err := db.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT listen_url, auth_token FROM quack_serve(%s)", args)).Scan(&url, &authToken); err != nil {
		return "", "", fmt.Errorf("quack serve: %w", err)
	}

	if e.opts.Lockdown {
		if err := applyLockdown(ctx, db, e.opts, "quack_serve", "quack_stop"); err != nil {
			return "", "", err
		}
	}
	return url, authToken, nil
}

func (e *Engine) openQuackBridge(ctx context.Context) (*sql.DB, error) {
	name := e.catalogs[0]
	bridgeID, token, err := e.registerBridge(ctx, name, e.opts.Perms.Catalogs[name], nil)
	if err != nil {
		return nil, fmt.Errorf("catalog %q: %w", name, err)
	}
	db, err := sql.Open("duckdb", bridgeExtension+":"+extensionDSN())
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, "SELECT vcatbridge_init(?, ?)", bridgeID, token); err != nil {
		return nil, errors.Join(fmt.Errorf("catalog %q: init bridge: %w", name, err), db.Close())
	}
	return db, nil
}

// collectQuackCatalog serves the source's main database, the only one a quack
// client's ATTACH sees; tables anywhere else would be silently unreachable.
func (e *Engine) collectQuackCatalog(ctx context.Context) error {
	var main string
	if err := e.source.QueryRowContext(ctx, `SELECT current_database()`).Scan(&main); err != nil {
		return err
	}

	rows, err := e.source.QueryContext(ctx, `
		SELECT database_name FROM duckdb_tables()
		WHERE database_name <> current_database() AND NOT internal
		GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var others []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		others = append(others, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(others) > 0 {
		return fmt.Errorf("quack serves only the main database %q, but the boot SQL left tables in %v: create them in %q, or name the catalog in --perms",
			main, others, main)
	}

	var tables int
	if err := e.source.QueryRowContext(ctx,
		`SELECT count(*) FROM duckdb_tables() WHERE database_name = ?`, main).Scan(&tables); err != nil {
		return err
	}
	e.log.Warnf("catalog %q serves %s with full read/write access (no perms.yml)",
		main, textfmt.Plural(tables, "table"))
	e.catalogs = []string{main}
	return nil
}
