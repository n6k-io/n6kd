// Package engine owns the DuckDB side: one source database holding the tables
// the schema file created, and one bridged database per client session.
//
// The source is never handed to a client: a session gets a separate database
// whose catalogs are attached TYPE virtual_catalog_bridge, granted the tables
// perms.yml names.
package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/duckdb/duckdb-go/v2"
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

// The source only answers bridge scans (virtual_catalog_bridge). A session is
// also the end of a client's socket, which needs n6k_client and n6k_server.
var (
	sourceExtensions  = []string{"virtual_catalog_bridge"}
	sessionExtensions = []string{"virtual_catalog_bridge", "n6k_client", "n6k_server"}
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

// Step is one unit of boot SQL: a file (-f) or inline statements (-c).
// Exactly one field is set.
type Step struct {
	Path string
	SQL  string
}

type Options struct {
	// Run into the source database in order, before anything is served.
	Boot []Step
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

	// Each holds the connection its body runs on, released by Close.
	tasks []*intervalTask

	// Closed to stop the n6kd_set_interval tickers; nil when none registered.
	done chan struct{}
	wg   sync.WaitGroup
}

// Open builds the source database and runs opts.Boot into it in order. Any
// error it returns aborts boot before the listener is created.
func Open(ctx context.Context, opts Options, log *zap.SugaredLogger) (*Engine, error) {
	// Direct mode serves the socket from this database, not from a session's.
	extensions := sourceExtensions
	if opts.Perms == nil {
		extensions = sessionExtensions
	}
	source, err := openWithExtensions(ctx, extensions, log)
	if err != nil {
		return nil, err
	}

	intervals := &setIntervalUDF{log: log}
	env := &getenvUDF{}
	for name, udf := range map[string]duckdb.ScalarFunc{
		setIntervalFunctionName: intervals,
		"getenv":                env,
		"n6kd_log":              &logUDF{log: log},
	} {
		if err := registerScalarUDF(ctx, source, name, udf); err != nil {
			return nil, errors.Join(err, source.Close())
		}
	}

	// Armed before the first boot statement: an n6kd_set_interval body runs
	// inline, while the statement that called it still holds its own connection.
	// Bodies may read the environment only where the source is not served to
	// clients, which is every mode but direct.
	bodyEnv := env
	if opts.Perms == nil {
		bodyEnv = nil
	}
	if err := intervals.arm(ctx, source, bodyEnv); err != nil {
		return nil, errors.Join(err, source.Close())
	}

	started := time.Now()
	if err := runBootSQL(ctx, source, opts.Boot, env, intervals, log); err != nil {
		return nil, errors.Join(err, closeAfterFailedBoot(intervals), source.Close())
	}
	log.Infof("boot SQL finished in %v", time.Since(started).Round(time.Millisecond))

	engine := &Engine{source: source, opts: opts, log: log}
	env.setArmed(false)
	tasks, err := intervals.disarm()
	if err != nil {
		return nil, errors.Join(err, source.Close())
	}

	if opts.Perms == nil {
		if err := engine.collectSourceCatalogs(ctx); err != nil {
			return nil, errors.Join(err, closeTasks(tasks), source.Close())
		}
		if opts.Lockdown {
			if err := applyLockdown(ctx, source); err != nil {
				return nil, errors.Join(err, closeTasks(tasks), source.Close())
			}
		}
		engine.startIntervalTasks(tasks)
		return engine, nil
	}
	engine.catalogs = slices.Sorted(maps.Keys(opts.Perms.Catalogs))
	if err := engine.verifyPermsAgainstSource(ctx); err != nil {
		return nil, errors.Join(err, closeTasks(tasks), source.Close())
	}
	engine.startIntervalTasks(tasks)
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
	if e.done != nil {
		close(e.done)
		e.wg.Wait()
	}
	return errors.Join(e.closeIntervalTasks(), e.source.Close())
}

// verifyPermsAgainstSource runs every check the bridge would otherwise run per session
// against the loaded schema: the table exists, and one granting update or delete resolves a
// primary key. Doing it here turns a perms.yml mistake into a failure to boot
// rather than an error on every client that connects. It also logs each
// resolved table and its access.
func (e *Engine) verifyPermsAgainstSource(ctx context.Context) error {
	total, keyed := 0, 0

	for _, name := range slices.Sorted(maps.Keys(e.opts.Perms.Catalogs)) {
		catalog := e.opts.Perms.Catalogs[name]
		e.log.Infof("catalog %q serves %s", name, plural(countTables(catalog), "table"))

		// Aligned on the name as printed, so the access column lines up.
		width := 0
		for schemaName, schema := range catalog.Schemas {
			for table := range schema.Tables {
				width = max(width, len(qualify(name, schemaName, table)))
			}
		}

		for _, schemaName := range slices.Sorted(maps.Keys(catalog.Schemas)) {
			schema := catalog.Schemas[schemaName]
			for _, table := range slices.Sorted(maps.Keys(schema.Tables)) {
				if err := e.requireSourceTable(ctx, name, schemaName, table); err != nil {
					return err
				}
				total++

				t := schema.Tables[table]
				verbs := strings.Join(t.Verbs(), ", ")
				if !t.NeedsKey() {
					e.log.Infof("  %-*s  %s", width, qualify(name, schemaName, table), verbs)
					continue
				}
				keyed++
				key, origin, err := e.resolvePrimaryKey(ctx, name, schemaName, table, t.Key)
				if err != nil {
					return err
				}
				e.log.Infof("  %-*s  %s, primary key %s (from %s)",
					width, qualify(name, schemaName, table), verbs, strings.Join(key, ", "), origin)
			}
		}
	}

	if keyed == 0 {
		e.log.Infof("perms check passed: %s found, none needing a primary key", plural(total, "table"))
		return nil
	}
	e.log.Infof("perms check passed: %s found, %s with a usable primary key",
		plural(total, "table"), plural(keyed, "updatable or deletable table"))
	return nil
}

func countTables(catalog perms.Catalog) int {
	n := 0
	for _, schema := range catalog.Schemas {
		n += len(schema.Tables)
	}
	return n
}

// requireSourceTable catches a table perms.yml names but the schema file never
// created.
func (e *Engine) requireSourceTable(ctx context.Context, catalog, schema, table string) error {
	var found int
	err := e.source.QueryRowContext(ctx,
		`SELECT count(*) FROM duckdb_tables()
		 WHERE database_name = ? AND schema_name = ? AND table_name = ?`,
		catalog, schema, table,
	).Scan(&found)
	if err != nil {
		return err
	}
	if found == 0 {
		return fmt.Errorf("perms.yml names %s, which the boot SQL did not create", qualify(catalog, schema, table))
	}
	return nil
}

// resolvePrimaryKey mirrors what bridge discovery does for a table granting
// update or delete: take the perms.yml override if there is one, otherwise discover the
// declared key, and refuse the table if neither yields columns. The override is
// additionally checked against duckdb_columns().
func (e *Engine) resolvePrimaryKey(ctx context.Context, catalog, schema, table string, override []string) ([]string, string, error) {
	if len(override) > 0 {
		for _, column := range override {
			var found int
			err := e.source.QueryRowContext(ctx,
				`SELECT count(*) FROM duckdb_columns()
				 WHERE database_name = ? AND schema_name = ? AND table_name = ?
				   AND lower(column_name) = lower(?)`,
				catalog, schema, table, column,
			).Scan(&found)
			if err != nil {
				return nil, "", err
			}
			if found == 0 {
				return nil, "", fmt.Errorf("key for %s names column %q, which that table does not have",
					qualify(catalog, schema, table), column)
			}
		}
		return override, "perms.yml", nil
	}

	var columns sql.NullString
	err := e.source.QueryRowContext(ctx,
		`SELECT array_to_string(constraint_column_names, ',') FROM duckdb_constraints()
		 WHERE database_name = ? AND schema_name = ? AND table_name = ?
		   AND constraint_type = 'PRIMARY KEY'`,
		catalog, schema, table,
	).Scan(&columns)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && columns.String == "") {
		return nil, "", fmt.Errorf("%s grants update or delete but has no primary key; declare one in the schema or set key in perms.yml",
			qualify(catalog, schema, table))
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
