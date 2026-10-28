package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/internal/sqlquote"
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
		if _, err := db.ExecContext(ctx, fmt.Sprintf("LOAD %s", sqlquote.String(target))); err != nil {
			return nil, errors.Join(fmt.Errorf("load %s: %w", name, err), db.Close())
		}
		log.Debugf("loaded extension %s from %s", name, target)
	}
	return db, nil
}

// openInstalledBridge loads vcatbridge by name rather than by path: a
// vcatbridge: database loads the installed copy as it opens, and a token only
// redeems against the copy that registered it. A local build from
// ExtensionDirEnv is installed over that copy first.
func openInstalledBridge(ctx context.Context, log *zap.SugaredLogger) (*sql.DB, error) {
	db, err := sql.Open("duckdb", extensionDSN())
	if err != nil {
		return nil, err
	}
	if os.Getenv(ExtensionDirEnv) != "" {
		target, err := ExtensionPath(bridgeExtension)
		if err != nil {
			return nil, errors.Join(err, db.Close())
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("FORCE INSTALL %s", sqlquote.String(target))); err != nil {
			return nil, errors.Join(fmt.Errorf("install %s: %w", bridgeExtension, err), db.Close())
		}
		log.Debugf("installed extension %s from %s", bridgeExtension, target)
	}
	if _, err := db.ExecContext(ctx, "LOAD "+bridgeExtension); err != nil {
		return nil, errors.Join(fmt.Errorf("load %s: %w", bridgeExtension, err), db.Close())
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
