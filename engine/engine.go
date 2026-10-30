// Package engine owns the DuckDB side: one source database holding the tables
// the boot SQL created, and how clients are served from it.
//
// With perms.yml, the source is never handed to a client: clients are served a
// database whose catalogs are attached TYPE vcatbridge, granted the tables
// perms.yml names (one per session, or the one ServeQuack serves). Without,
// every client is served the source itself.
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
	"strconv"
	"sync"
	"time"

	"github.com/duckdb/duckdb-go/v2"
	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/internal/sqlquote"
	"github.com/n6k-io/n6kd/internal/textfmt"
	"github.com/n6k-io/n6kd/perms"
)

const ExtensionDirEnv = "N6K_EXTENSION_DIR"

// ExtensionHomeEnv points DuckDB's own extension_directory at a baked install —
// the container sets it so a released build loads by name with no local build
// and no network. Unrelated to ExtensionDirEnv, which names a different layout.
const ExtensionHomeEnv = "N6K_EXTENSION_HOME"

// bundledExtensionDir is the baked install a release archive ships beside the
// binary, used when ExtensionHomeEnv is unset.
const bundledExtensionDir = "extensions"

const ExtensionRepoEnv = "N6K_EXTENSION_REPO"

const defaultExtensionRepo = "https://storage.googleapis.com/n6k-duckdb-release"

// The source only answers bridge scans (vcatbridge). A session is
// also the end of a client's socket, which needs n6k_client and n6k_server.
var (
	sourceExtensions  = []string{bridgeExtension}
	sessionExtensions = []string{bridgeExtension, "n6k_client", "n6k_server"}
)

const bridgeExtension = "vcatbridge"

// Quack serves over these, from DuckDB's own repository; httpfs must be loaded
// before lockdown takes extension loading away.
var quackExtensions = []string{"quack", "httpfs"}

// InstallExtensions downloads every extension n6kd or quack loads into DuckDB's
// extension_directory.
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
		// operator's environment; both are escaped by sqlquote.String.
		stmt := fmt.Sprintf("INSTALL %s FROM %s", sqlquote.String(name), sqlquote.String(repo))
		// #nosec G701 -- stmt is built from the constant list and a quoted repo above.
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("install %s: %w", name, err)
		}
		log.Infof("installed extension %s from %s", name, repo)
	}
	for _, name := range quackExtensions {
		// #nosec G201 -- name comes from quackExtensions, escaped by sqlquote.String.
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSTALL %s", sqlquote.String(name))); err != nil {
			return fmt.Errorf("install %s: %w", name, err)
		}
		log.Infof("installed extension %s", name)
	}
	return nil
}

// The n6k extensions are unsigned, so loading them needs allow_unsigned_extensions.
func extensionDSN() string {
	dsn := "?allow_unsigned_extensions=true"
	if home := extensionHome(); home != "" {
		dsn += "&extension_directory=" + url.QueryEscape(home)
	}
	return dsn
}

// extensionHome is "" when DuckDB's default extension_directory applies.
func extensionHome() string {
	if home := os.Getenv(ExtensionHomeEnv); home != "" {
		return home
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	// A symlink on PATH must still find the extensions beside the real binary.
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return ""
	}
	dir := filepath.Join(filepath.Dir(exe), bundledExtensionDir)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return ""
	}
	return dir
}

// sessionDSN opens a session's database at the limits lockdown would lock in, so
// a client never holds more than them, locked down or not.
func sessionDSN(opts Options) string {
	dsn := extensionDSN()
	if opts.MemoryLimit != "" {
		dsn += "&memory_limit=" + url.QueryEscape(opts.MemoryLimit)
	}
	if opts.Threads > 0 {
		dsn += "&threads=" + strconv.Itoa(opts.Threads)
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
	Boot  []Step
	Perms *perms.File
	// Ignored when Verifier is set. Otherwise empty means no authentication, and
	// clients must present this token.
	APIKey   string
	Verifier TokenVerifier
	// Lockdown takes the local filesystem and extension installation away from
	// served connections.
	Lockdown bool
	// Passed to vcatbridge_lockdown, per served DatabaseInstance (see Session
	// and ServeQuack for which one that is), and set on each session's database
	// as it opens. Empty or zero keeps that database's current setting.
	MemoryLimit string
	Threads     int
	// Serve over quack (ServeQuack) instead of n6k sessions. With Perms, it
	// must name exactly one catalog: quack clients see only the main database.
	Quack bool
}

type Engine struct {
	source   *sql.DB
	opts     Options
	catalogs []string
	log      *zap.SugaredLogger

	// The vcatbridge: database ServeQuack serves; nil when it serves the source.
	quack *sql.DB

	// Each holds the connection its body runs on, released by Close.
	tasks []*intervalTask

	// Closed to stop the n6kd_set_interval tickers; nil when none registered.
	done chan struct{}
	wg   sync.WaitGroup
}

// Open builds the source database and runs opts.Boot into it in order.
func Open(ctx context.Context, opts Options, log *zap.SugaredLogger) (*Engine, error) {
	if opts.Quack && opts.Perms != nil && len(opts.Perms.Catalogs) != 1 {
		return nil, fmt.Errorf("quack serves one catalog, perms.yml names %d", len(opts.Perms.Catalogs))
	}

	var source *sql.DB
	var err error
	if opts.Quack {
		source, err = openInstalledBridge(ctx, log)
	} else {
		// Direct mode serves the socket from this database, not from a session's.
		extensions := sourceExtensions
		if opts.Perms == nil {
			extensions = sessionExtensions
		}
		source, err = openWithExtensions(ctx, extensionDSN(), extensions, log)
	}
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
		return nil, errors.Join(err, closeTasks(tasks), source.Close())
	}

	if opts.Perms == nil {
		collect := engine.collectSourceCatalogs
		if opts.Quack {
			collect = engine.collectQuackCatalog
		}
		if err := collect(ctx); err != nil {
			return nil, errors.Join(err, closeTasks(tasks), source.Close())
		}
		// Quack locks down after quack_serve, in ServeQuack.
		if opts.Lockdown && !opts.Quack {
			if err := applyLockdown(ctx, source, opts); err != nil {
				return nil, errors.Join(err, closeTasks(tasks), source.Close())
			}
		}
		engine.startIntervalTasks(tasks)
		return engine, nil
	}
	engine.catalogs = slices.Sorted(maps.Keys(opts.Perms.Catalogs))
	if err := engine.validatePolicies(ctx); err != nil {
		return nil, errors.Join(err, closeTasks(tasks), source.Close())
	}
	engine.startIntervalTasks(tasks)
	return engine, nil
}

// validatePolicies fails boot on any policy setting the bridge would refuse
// against the source as loaded; the bridge re-checks when a catalog is registered.
func (e *Engine) validatePolicies(ctx context.Context) error {
	var failures []error
	for _, name := range e.catalogs {
		rows, err := e.source.QueryContext(ctx,
			`SELECT coalesce(path, '<document>'), severity, message FROM vcatbridge_validate_policy_config(?, ?)`,
			name, string(e.opts.Perms.Catalogs[name]))
		if err != nil {
			return fmt.Errorf("catalog %q: validate policy: %w", name, err)
		}
		for rows.Next() {
			var path, severity, message string
			if err := rows.Scan(&path, &severity, &message); err != nil {
				_ = rows.Close()
				return err
			}
			if severity == "error" {
				failures = append(failures, fmt.Errorf("catalog %q: %s: %s", name, path, message))
			} else {
				e.log.Warnf("catalog %q: %s (%s): %s", name, path, severity, message)
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("perms.yml: %w", errors.Join(failures...))
	}
	e.log.Infof("perms check passed: %s", textfmt.Plural(len(e.catalogs), "catalog"))
	return nil
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
			name, textfmt.Plural(tables, "table"))
	}
	return nil
}

func (e *Engine) Close() error {
	if e.done != nil {
		close(e.done)
		e.wg.Wait()
	}
	var quackErr error
	if e.quack != nil {
		quackErr = e.quack.Close()
	}
	return errors.Join(quackErr, e.closeIntervalTasks(), e.source.Close())
}

func (e *Engine) Catalogs() []string {
	return e.catalogs
}
