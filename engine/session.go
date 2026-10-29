package engine

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/internal/sqlquote"
	"github.com/n6k-io/n6kd/internal/textfmt"
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
		if err := applyLockdown(ctx, s.db, e.opts); err != nil {
			return err
		}
	}
	log.Debugf("session ready with %s in %v",
		textfmt.Plural(len(e.opts.Perms.Catalogs), "catalog"), time.Since(started).Round(time.Millisecond))
	return nil
}

func (e *Engine) attachCatalog(ctx context.Context, db *sql.DB, name string, policy json.RawMessage, values map[string]any) error {
	bridgeID, token, err := e.registerBridge(ctx, name, policy, values)
	if err != nil {
		return err
	}

	// ATTACH options fold to constants at bind time; ? placeholders are rejected.
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("ATTACH '' AS %s (TYPE vcatbridge, ID %s, TOKEN %s)",
			sqlquote.Ident(name), sqlquote.String(bridgeID), sqlquote.String(token))); err != nil {
		return fmt.Errorf("attach bridge: %w", err)
	}
	return nil
}

// The token expires after 30s idle, so nothing slow may sit between this and
// redeeming it.
func (e *Engine) registerBridge(ctx context.Context, name string, policy json.RawMessage, values map[string]any) (bridgeID, token string, err error) {
	bridgeID, err = randomHex()
	if err != nil {
		return "", "", err
	}

	if err := e.source.QueryRowContext(ctx,
		"SELECT vcatbridge_register_source(?, ?)", bridgeID, name,
	).Scan(&token); err != nil {
		return "", "", fmt.Errorf("register source: %w", err)
	}

	for _, key := range slices.Sorted(maps.Keys(values)) {
		if _, err := e.source.ExecContext(ctx,
			"SELECT vcatbridge_set_context(?, ?, ?)", bridgeID, key, values[key]); err != nil {
			return "", "", fmt.Errorf("context %s: %w", key, err)
		}
	}

	if _, err := e.source.ExecContext(ctx,
		"SELECT vcatbridge_configure_policy(?, ?)", bridgeID, string(policy)); err != nil {
		return "", "", fmt.Errorf("policy: %w", err)
	}

	if _, err := e.source.ExecContext(ctx, "SELECT vcatbridge_finalize_context(?)", bridgeID); err != nil {
		return "", "", fmt.Errorf("finalize context: %w", err)
	}
	return bridgeID, token, nil
}

func randomHex() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
