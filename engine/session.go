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

// The token expires after 30s idle, so nothing slow may sit between register and ATTACH.
func (e *Engine) attachCatalog(ctx context.Context, db *sql.DB, name string, catalog perms.Catalog) error {
	bridgeID, err := newBridgeID()
	if err != nil {
		return err
	}

	var token string
	if err := e.source.QueryRowContext(ctx,
		"SELECT bridge_register_source(?, ?)", bridgeID, name,
	).Scan(&token); err != nil {
		return fmt.Errorf("register source: %w", err)
	}

	for _, schemaName := range slices.Sorted(maps.Keys(catalog.Schemas)) {
		schema := catalog.Schemas[schemaName]
		for _, table := range slices.Sorted(maps.Keys(schema.Tables)) {
			qualified := schemaName + "." + table
			if key := schema.Tables[table].Key; len(key) > 0 {
				if _, err := e.source.ExecContext(ctx,
					fmt.Sprintf("SELECT bridge_primary_key(?, ?, %s)", listLiteral(key)),
					bridgeID, qualified); err != nil {
					return fmt.Errorf("primary key %s: %w", qualified, err)
				}
			}
			for _, g := range schema.Tables[table].Grants {
				// A NULL fifth argument would make DuckDB skip the call entirely, not pass NULL.
				stmt, args := "SELECT bridge_policy(?, ?, ?, ?)", []any{bridgeID, qualified, g.Verb, g.Using}
				if g.Check != "" {
					stmt, args = "SELECT bridge_policy(?, ?, ?, ?, ?)", append(args, g.Check)
				}
				if _, err := e.source.ExecContext(ctx, stmt, args...); err != nil {
					return fmt.Errorf("grant %s %s: %w", g.Verb, qualified, err)
				}
			}
		}
	}

	// ATTACH options fold to constants at bind time; ? placeholders are rejected.
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("ATTACH '' AS %s (TYPE virtual_catalog_bridge, ID %s, TOKEN %s)",
			quoteIdent(name), quoteString(bridgeID), quoteString(token))); err != nil {
		return fmt.Errorf("attach bridge: %w", err)
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
