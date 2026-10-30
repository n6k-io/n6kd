package engine

import (
	"context"
	"database/sql"

	"go.uber.org/zap"
)

func OpenClient(ctx context.Context, log *zap.SugaredLogger) (*sql.DB, error) {
	return openWithExtensions(ctx, extensionDSN(), []string{"n6k_client"}, log)
}
