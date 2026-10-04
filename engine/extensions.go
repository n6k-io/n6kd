package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"
)

func openWithExtensions(ctx context.Context, extensions []string, log *zap.SugaredLogger) (*sql.DB, error) {
	db, err := sql.Open("duckdb", extensionDSN())
	if err != nil {
		return nil, err
	}
	for _, name := range extensions {
		target, err := ExtensionPath(name)
		if err != nil {
			return nil, errors.Join(err, db.Close())
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("LOAD %s", quoteString(target))); err != nil {
			return nil, errors.Join(fmt.Errorf("load %s: %w", name, err), db.Close())
		}
		log.Debugf("loaded extension %s from %s", name, target)
	}
	return db, nil
}

// ExtensionPath returns the first <dir>/<name>/<name>.duckdb_extension found
// under the ExtensionDirEnv path list, and otherwise the bare name, which LOAD
// resolves against DuckDB's extension_directory — what InstallExtensions populated.
func ExtensionPath(name string) (string, error) {
	dirs := filepath.SplitList(os.Getenv(ExtensionDirEnv))
	if len(dirs) == 0 {
		return name, nil
	}
	var errs []error
	for _, dir := range dirs {
		path := filepath.Join(dir, name, name+".duckdb_extension")
		// #nosec G703 -- dir comes from the operator's environment, name from a constant list.
		_, err := os.Stat(path)
		if err == nil {
			return path, nil
		}
		errs = append(errs, err)
	}
	return "", fmt.Errorf("extension %s: %w", name, errors.Join(errs...))
}
