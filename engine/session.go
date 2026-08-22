package engine

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/perms"
)

// Session is one client's view: its own database, carrying only the bridged
// catalogs. Closing it drops that database; the source outlives it.
//
// In direct mode the database is the shared source, which the engine owns.
type Session struct {
	db     *sql.DB
	shared bool
}

func (s *Session) DB() *sql.DB { return s.db }

func (s *Session) Close() error {
	if s.shared {
		return nil
	}
	return s.db.Close()
}

// NewSession attaches every catalog in perms.yml as a bridge over the source.
// log is the caller's per-client logger.
func (e *Engine) NewSession(ctx context.Context, log *zap.SugaredLogger) (*Session, error) {
	if e.opts.Perms == nil {
		log.Debugf("session serves the source database directly")
		return &Session{db: e.source, shared: true}, nil
	}

	started := time.Now()
	db, err := openWithExtensions(ctx, sessionExtensions, log)
	if err != nil {
		return nil, err
	}
	if e.opts.APIKey != "" {
		if err := registerAuthorizeUDF(ctx, db, e.opts.APIKey); err != nil {
			return nil, errors.Join(err, db.Close())
		}
	}
	for _, name := range slices.Sorted(maps.Keys(e.opts.Perms.Catalogs)) {
		if err := e.attachCatalog(ctx, db, name, e.opts.Perms.Catalogs[name]); err != nil {
			return nil, errors.Join(fmt.Errorf("catalog %q: %w", name, err), db.Close())
		}
		log.Debugf("bridged catalog %q for this client", name)
	}
	if e.opts.Lockdown {
		if err := applyLockdown(ctx, db); err != nil {
			return nil, errors.Join(err, db.Close())
		}
	}
	log.Debugf("session ready with %s in %v",
		plural(len(e.opts.Perms.Catalogs), "catalog"), time.Since(started).Round(time.Millisecond))
	return &Session{db: db}, nil
}

// attachCatalog attaches the catalog once and bridges each of its schemas into
// it. vcat_register_source takes a single source catalog.schema and bare table
// names, so a schema is the unit a bridge covers.
func (e *Engine) attachCatalog(ctx context.Context, db *sql.DB, name string, catalog perms.Catalog) error {
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("ATTACH ':memory:' AS %s (TYPE virtual_catalog)", quoteIdent(name))); err != nil {
		return err
	}
	for _, schemaName := range slices.Sorted(maps.Keys(catalog.Schemas)) {
		if err := e.bridgeSchema(ctx, db, name, schemaName, catalog.Schemas[schemaName]); err != nil {
			return fmt.Errorf("schema %q: %w", schemaName, err)
		}
	}
	return nil
}

// bridgeSchema runs the two-sided handshake: the source registers the tables and
// primary keys it exposes and returns a token, the target redeems it.
func (e *Engine) bridgeSchema(ctx context.Context, db *sql.DB, catalog, schemaName string, schema perms.Schema) error {
	bridgeID, err := newBridgeID()
	if err != nil {
		return err
	}

	var token string
	err = e.source.QueryRowContext(ctx,
		fmt.Sprintf("SELECT vcat_register_source(?, ?, ?, %s, %s)",
			mapLiteral(schema.Tables), mapLiteral(primaryKeyMap(schema.PrimaryKeys))),
		bridgeID, catalog, schemaName,
	).Scan(&token)
	if err != nil {
		return fmt.Errorf("register source: %w", err)
	}

	// The bridge requires the target schema to exist before it is set up.
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s.%s", quoteIdent(catalog), quoteIdent(schemaName))); err != nil {
		return err
	}

	var status string
	err = db.QueryRowContext(ctx,
		"SELECT vcat_setup_bridge(?, ?, ?, ?)",
		bridgeID, token, catalog, schemaName,
	).Scan(&status)
	if err != nil {
		return fmt.Errorf("setup bridge: %w", err)
	}
	if status != "ok" {
		return fmt.Errorf("setup bridge: %s", status)
	}
	return nil
}

func newBridgeID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
