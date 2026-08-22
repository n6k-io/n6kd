// Package engine owns the DuckDB side: one source database holding the tables
// the schema file created, and one bridged database per client session.
//
// The source is never handed to a client: a session gets a separate database
// whose catalogs are attached TYPE virtual_catalog and populated by
// vcat_setup_bridge from the tables perms.yml names.
package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/perms"
)

// ExtensionDirEnv mirrors the Python loader, so both find the same local build.
const ExtensionDirEnv = "N6K_EXTENSION_DIR"

// ExtensionHomeEnv points DuckDB's own extension_directory at a baked install —
// the container sets it so a released build loads by name with no local build
// and no network. Unrelated to ExtensionDirEnv, which names a different layout.
const ExtensionHomeEnv = "N6K_EXTENSION_HOME"

// ExtensionRepoEnv overrides the release bucket, which is the same one the
// Python loader installs from.
const ExtensionRepoEnv = "N6K_EXTENSION_REPO"

const defaultExtensionRepo = "https://storage.googleapis.com/n6k-duckdb-release"

// The source only answers bridge scans (virtual_catalog). A session is also
// the end of a client's socket, which needs n6k_client and n6k_server.
var (
	sourceExtensions  = []string{"virtual_catalog"}
	sessionExtensions = []string{"virtual_catalog", "n6k_client", "n6k_server"}
)

// InstallExtensions downloads every extension a session can need into DuckDB's
// extension_directory. The container image runs this at build time (Dockerfile).
func InstallExtensions(ctx context.Context, log *zap.SugaredLogger) error {
	db, err := sql.Open("duckdb", extensionDSN())
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	repo := os.Getenv(ExtensionRepoEnv)
	if repo == "" {
		repo = defaultExtensionRepo
	}
	for _, name := range sessionExtensions {
		// #nosec G201 -- name comes from sessionExtensions, repo from the
		// operator's environment; both are escaped by quoteString.
		stmt := fmt.Sprintf("INSTALL %s FROM %s", quoteString(name), quoteString(repo))
		// #nosec G701 -- stmt is built from the constant list and a quoted repo above.
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("install %s: %w", name, err)
		}
		log.Infof("installed extension %s from %s", name, repo)
	}
	return nil
}

// The n6k extensions are unsigned, so loading them needs allow_unsigned_extensions.
func extensionDSN() string {
	dsn := "?allow_unsigned_extensions=true"
	if home := os.Getenv(ExtensionHomeEnv); home != "" {
		dsn += "&extension_directory=" + url.QueryEscape(home)
	}
	return dsn
}

type Options struct {
	SchemaPath string
	// nil means no perms.yml: sessions share the source directly, unbridged.
	Perms *perms.File
	// Empty means no authentication; otherwise clients must present this token.
	APIKey string
	// Lockdown takes the local filesystem and extension installation away from
	// served connections.
	Lockdown bool
}

type Engine struct {
	source   *sql.DB
	opts     Options
	catalogs []string
	log      *zap.SugaredLogger
}

// Open builds the source database and runs opts.SchemaPath into it. Any error
// it returns aborts boot before the listener is created.
func Open(ctx context.Context, opts Options, log *zap.SugaredLogger) (*Engine, error) {
	// #nosec G304 -- the operator names this file on the command line.
	schema, err := os.ReadFile(opts.SchemaPath)
	if err != nil {
		return nil, err
	}

	// Direct mode serves the socket from this database, not from a session's.
	extensions := sourceExtensions
	if opts.Perms == nil {
		extensions = sessionExtensions
	}
	source, err := openWithExtensions(ctx, extensions, log)
	if err != nil {
		return nil, err
	}

	started := time.Now()
	if _, err := source.ExecContext(ctx, string(schema)); err != nil {
		return nil, errors.Join(fmt.Errorf("%s: %w", opts.SchemaPath, err), source.Close())
	}
	log.Infof("loaded schema %s (%d bytes) in %v",
		opts.SchemaPath, len(schema), time.Since(started).Round(time.Millisecond))

	engine := &Engine{source: source, opts: opts, log: log}
	if opts.Perms == nil {
		if opts.APIKey != "" {
			if err := registerAuthorizeUDF(ctx, source, opts.APIKey); err != nil {
				return nil, errors.Join(err, source.Close())
			}
		}
		if err := engine.collectSourceCatalogs(ctx); err != nil {
			return nil, errors.Join(err, source.Close())
		}
		if opts.Lockdown {
			if err := applyLockdown(ctx, source); err != nil {
				return nil, errors.Join(err, source.Close())
			}
		}
		return engine, nil
	}
	engine.catalogs = slices.Sorted(maps.Keys(opts.Perms.Catalogs))
	if err := engine.verifyPermsAgainstSource(ctx); err != nil {
		return nil, errors.Join(err, source.Close())
	}
	return engine, nil
}

func (e *Engine) collectSourceCatalogs(ctx context.Context) error {
	rows, err := e.source.QueryContext(ctx,
		`SELECT database_name FROM duckdb_databases() WHERE NOT internal ORDER BY 1`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		e.catalogs = append(e.catalogs, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(e.catalogs) == 0 {
		return errors.New("no catalogs to serve: the schema left no attached database")
	}

	for _, name := range e.catalogs {
		var tables int
		if err := e.source.QueryRowContext(ctx,
			`SELECT count(*) FROM duckdb_tables() WHERE database_name = ?`, name).Scan(&tables); err != nil {
			return err
		}
		e.log.Warnf("catalog %q serves %s with full read/write access (no perms.yml)",
			name, plural(tables, "table"))
	}
	return nil
}

func (e *Engine) Close() error {
	return e.source.Close()
}

// verifyPermsAgainstSource runs every check the bridge would otherwise run per session
// against the loaded schema: the table exists, and a writable one resolves a
// primary key. Doing it here turns a perms.yml mistake into a failure to boot
// rather than an error on every client that connects. It also logs each
// resolved table and its access.
func (e *Engine) verifyPermsAgainstSource(ctx context.Context) error {
	source := e.opts.Perms.Source.Catalog + "." + e.opts.Perms.Source.Schema
	total, writable := 0, 0

	for _, name := range slices.Sorted(maps.Keys(e.opts.Perms.Catalogs)) {
		catalog := e.opts.Perms.Catalogs[name]
		tables := slices.Sorted(maps.Keys(catalog.Tables))
		e.log.Infof("catalog %q serves %s from %s", name, plural(len(tables), "table"), source)

		// Aligned on the name as printed, so the access column lines up.
		width := 0
		for _, table := range tables {
			width = max(width, len(qualify(name, catalog.Schema, table)))
		}

		for _, table := range tables {
			if err := e.requireSourceTable(ctx, name, table); err != nil {
				return err
			}
			total++

			if catalog.Tables[table] != perms.ReadWrite {
				e.log.Infof("  %-*s  read", width, qualify(name, catalog.Schema, table))
				continue
			}
			writable++
			key, origin, err := e.resolvePrimaryKey(ctx, name, table, catalog.PrimaryKeys[table])
			if err != nil {
				return err
			}
			e.log.Infof("  %-*s  readwrite, primary key %s (from %s)",
				width, qualify(name, catalog.Schema, table), strings.Join(key, ", "), origin)
		}
	}

	if writable == 0 {
		e.log.Infof("perms check passed: %s found in %s, none writable", plural(total, "table"), source)
		return nil
	}
	e.log.Infof("perms check passed: %s found in %s, %s with a usable primary key",
		plural(total, "table"), source, plural(writable, "writable table"))
	return nil
}

// requireSourceTable catches a table perms.yml names but the schema file never
// created.
func (e *Engine) requireSourceTable(ctx context.Context, catalog, table string) error {
	var found int
	err := e.source.QueryRowContext(ctx,
		`SELECT count(*) FROM duckdb_tables()
		 WHERE database_name = ? AND schema_name = ? AND table_name = ?`,
		e.opts.Perms.Source.Catalog, e.opts.Perms.Source.Schema, table,
	).Scan(&found)
	if err != nil {
		return err
	}
	if found == 0 {
		return fmt.Errorf("catalog %q: table %q is not in %s.%s",
			catalog, table, e.opts.Perms.Source.Catalog, e.opts.Perms.Source.Schema)
	}
	return nil
}

// resolvePrimaryKey mirrors what vcat_register_source does for a writable
// table: take the perms.yml override if there is one, otherwise discover the
// declared key, and refuse the table if neither yields columns. The override is
// additionally checked against duckdb_columns().
func (e *Engine) resolvePrimaryKey(ctx context.Context, catalog, table string, override []string) ([]string, string, error) {
	if len(override) > 0 {
		for _, column := range override {
			var found int
			err := e.source.QueryRowContext(ctx,
				`SELECT count(*) FROM duckdb_columns()
				 WHERE database_name = ? AND schema_name = ? AND table_name = ?
				   AND lower(column_name) = lower(?)`,
				e.opts.Perms.Source.Catalog, e.opts.Perms.Source.Schema, table, column,
			).Scan(&found)
			if err != nil {
				return nil, "", err
			}
			if found == 0 {
				return nil, "", fmt.Errorf("catalog %q: primary_keys for %q names column %q, which %s.%s.%s does not have",
					catalog, table, column, e.opts.Perms.Source.Catalog, e.opts.Perms.Source.Schema, table)
			}
		}
		return override, "perms.yml", nil
	}

	var columns sql.NullString
	err := e.source.QueryRowContext(ctx,
		`SELECT array_to_string(constraint_column_names, ',') FROM duckdb_constraints()
		 WHERE database_name = ? AND schema_name = ? AND table_name = ?
		   AND constraint_type = 'PRIMARY KEY'`,
		e.opts.Perms.Source.Catalog, e.opts.Perms.Source.Schema, table,
	).Scan(&columns)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && columns.String == "") {
		return nil, "", fmt.Errorf("catalog %q: table %q is readwrite but has no primary key; declare one in the schema or add a primary_keys override",
			catalog, table)
	}
	if err != nil {
		return nil, "", err
	}
	return strings.Split(columns.String, ","), "schema", nil
}

// Catalogs names what a session will carry, for logging at accept time.
func (e *Engine) Catalogs() []string {
	return e.catalogs
}

func openWithExtensions(ctx context.Context, extensions []string, log *zap.SugaredLogger) (*sql.DB, error) {
	db, err := sql.Open("duckdb", extensionDSN())
	if err != nil {
		return nil, err
	}
	for _, name := range extensions {
		target, err := extensionLoadArg(name)
		if err != nil {
			return nil, errors.Join(err, db.Close())
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("LOAD %s", quoteString(target))); err != nil {
			return nil, errors.Join(fmt.Errorf("load %s: %w", name, err), db.Close())
		}
		log.Debugf("loaded extension %s from %s", name, target)
	}
	return db, nil
}

// extensionLoadArg returns a path under ExtensionDirEnv when that is set, and
// otherwise the bare name, which LOAD resolves against DuckDB's
// extension_directory — what InstallExtensions populated.
func extensionLoadArg(name string) (string, error) {
	dir := os.Getenv(ExtensionDirEnv)
	if dir == "" {
		return name, nil
	}
	path := filepath.Join(dir, name, name+".duckdb_extension")
	// #nosec G703 -- dir comes from the operator's environment, name from a constant list.
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("extension %s: %w", name, err)
	}
	return path, nil
}
