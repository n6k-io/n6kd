package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/n6k-io/n6kd/internal/sqlquote"
)

// applyLockdown must be called after the schema load, extension loads and
// bridge setup — see the call sites in engine.go, session.go and quack.go.
// drop names system functions removed from the catalog first.
func applyLockdown(ctx context.Context, db *sql.DB, opts Options, drop ...string) error {
	dropList := ""
	if len(drop) > 0 {
		quoted := make([]string, len(drop))
		for i, name := range drop {
			quoted[i] = sqlquote.String(name)
		}
		dropList = ", [" + strings.Join(quoted, ", ") + "]"
	}
	// #nosec G201 -- dropList holds only names quoted by sqlquote.String.
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`SELECT vcatbridge_lockdown(
		coalesce(nullif(?, ''), current_setting('memory_limit')),
		coalesce(nullif(?, 0), current_setting('threads')::BIGINT)%s)`, dropList),
		opts.MemoryLimit, opts.Threads); err != nil {
		return fmt.Errorf("lockdown: %w", err)
	}
	return nil
}
