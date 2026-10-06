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

func (e *Engine) NewSession(ctx context.Context, log *zap.SugaredLogger) (*Session, error) {
	s, err := e.OpenSession(ctx, log)
	if err != nil {
		return nil, err
	}
	if err := e.Admit(ctx, s, nil, log); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	return s, nil
}

func (e *Engine) OpenSession(ctx context.Context, log *zap.SugaredLogger) (*Session, error) {
	if e.opts.Perms == nil {
		log.Debugf("session serves the source database directly")
		return &Session{db: e.source, shared: true}, nil
	}
	db, err := openWithExtensions(ctx, sessionExtensions, log)
	if err != nil {
		return nil, err
	}
	return &Session{db: db}, nil
}

func (e *Engine) Admit(ctx context.Context, s *Session, values map[string]any, log *zap.SugaredLogger) error {
	if s.shared {
		return nil
	}
	started := time.Now()
	for _, name := range slices.Sorted(maps.Keys(e.opts.Perms.Catalogs)) {
		if err := e.attachCatalog(ctx, s.db, name, e.opts.Perms.Catalogs[name], values); err != nil {
			return fmt.Errorf("catalog %q: %w", name, err)
		}
		log.Debugf("bridged catalog %q for this client", name)
	}
	if e.opts.Lockdown {
		if err := applyLockdown(ctx, s.db); err != nil {
			return err
		}
	}
	log.Debugf("session ready with %s in %v",
		plural(len(e.opts.Perms.Catalogs), "catalog"), time.Since(started).Round(time.Millisecond))
	return nil
}

// The token expires after 30s idle, so nothing slow may sit between register and ATTACH.
func (e *Engine) attachCatalog(ctx context.Context, db *sql.DB, name string, catalog perms.Catalog, values map[string]any) error {
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

	for _, key := range slices.Sorted(maps.Keys(values)) {
		if _, err := e.source.ExecContext(ctx,
			"SELECT bridge_set_context(?, ?, ?)", bridgeID, key, values[key]); err != nil {
			return fmt.Errorf("context %s: %w", key, err)
		}
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

	if _, err := e.source.ExecContext(ctx, "SELECT bridge_finalize_context(?)", bridgeID); err != nil {
		return fmt.Errorf("finalize context: %w", err)
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
