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
		if err := e.attachBridge(ctx, db, name, e.opts.Perms.Catalogs[name]); err != nil {
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

// attachBridge runs the two-sided handshake: the source registers the tables and
// primary keys it exposes and returns a token, the target redeems it.
func (e *Engine) attachBridge(ctx context.Context, db *sql.DB, name string, catalog perms.Catalog) error {
	bridgeID, err := newBridgeID()
	if err != nil {
		return err
	}

	var token string
	err = e.source.QueryRowContext(ctx,
		fmt.Sprintf("SELECT n6k_bridge_register_source(?, ?, ?, %s, %s)",
			mapLiteral(catalog.Tables), mapLiteral(primaryKeyMap(catalog.PrimaryKeys))),
		bridgeID, e.opts.Perms.Source.Catalog, e.opts.Perms.Source.Schema,
	).Scan(&token)
	if err != nil {
		return fmt.Errorf("register source: %w", err)
	}

	stmts := []string{
		fmt.Sprintf("ATTACH ':memory:' AS %s (TYPE n6k_bridge)", quoteIdent(name)),
		// The bridge requires the target schema to exist before it is set up.
		fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s.%s", quoteIdent(name), quoteIdent(catalog.Schema)),
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}

	var status string
	err = db.QueryRowContext(ctx,
		"SELECT n6k_bridge_setup_bridge(?, ?, ?, ?)",
		bridgeID, token, name, catalog.Schema,
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
