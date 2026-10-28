package engine

import (
	"context"
	"database/sql"
	"fmt"
)

// applyLockdown must be called after the schema load, extension loads and
// bridge setup — see the call sites in engine.go and session.go.
func applyLockdown(ctx context.Context, db *sql.DB, opts Options) error {
	if _, err := db.ExecContext(ctx, `SELECT vcatbridge_lockdown(
		coalesce(nullif(?, ''), current_setting('memory_limit')),
		coalesce(nullif(?, 0), current_setting('threads')::BIGINT))`,
		opts.MemoryLimit, opts.Threads); err != nil {
		return fmt.Errorf("lockdown: %w", err)
	}
	return nil
}
