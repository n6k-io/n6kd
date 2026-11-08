package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.uber.org/zap"
)

// OpenClient loads httpfs because n6k_client refreshes tokens with POST, which
// DuckDB only implements once httpfs is loaded.
func OpenClient(ctx context.Context, log *zap.SugaredLogger) (*sql.DB, error) {
	db, err := openWithExtensions(ctx, extensionDSN(), []string{"n6k_client"}, log)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, "INSTALL httpfs; LOAD httpfs"); err != nil {
		return nil, errors.Join(fmt.Errorf("load httpfs: %w", err), db.Close())
	}
	return db, nil
}
