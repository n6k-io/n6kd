package engine

import (
	"context"
	"database/sql"
	"fmt"
)

// Order matters: lock_configuration=true blocks further SET, so it comes last.
var lockdownSettings = []string{
	"SET enable_external_access=false",
	"SET autoinstall_known_extensions=false",
	"SET autoload_known_extensions=false",
	"SET allow_community_extensions=false",
	"SET lock_configuration=true",
}

// applyLockdown must be called after the schema load, extension loads and
// bridge setup — see the call sites in engine.go and session.go.
func applyLockdown(ctx context.Context, db *sql.DB) error {
	for _, stmt := range lockdownSettings {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("lockdown: %s: %w", stmt, err)
		}
	}
	return nil
}
