package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
)

type ExtensionVersion struct {
	Name string
	// Version is "" when Path is a local build from ExtensionDirEnv, which DuckDB
	// does not report a version for until it is loaded.
	Version string
	Path    string
}

type VersionInfo struct {
	DuckDB       string
	DuckDBSource string
	ExtensionDir string
	Extensions   []ExtensionVersion
}

// Versions reports on the extensions without loading them: ExtensionDirEnv
// builds by path, the rest from duckdb_extensions(), Path "" meaning not installed.
func Versions(ctx context.Context) (_ VersionInfo, err error) {
	var info VersionInfo
	db, err := sql.Open("duckdb", extensionDSN())
	if err != nil {
		return info, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()

	row := db.QueryRowContext(ctx, "SELECT library_version, source_id, current_setting('extension_directory') FROM pragma_version()")
	if err := row.Scan(&info.DuckDB, &info.DuckDBSource, &info.ExtensionDir); err != nil {
		return info, fmt.Errorf("duckdb version: %w", err)
	}

	installed := map[string]ExtensionVersion{}
	rows, err := db.QueryContext(ctx, "SELECT extension_name, coalesce(extension_version, ''), coalesce(install_path, '') FROM duckdb_extensions() WHERE installed")
	if err != nil {
		return info, fmt.Errorf("duckdb_extensions: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var e ExtensionVersion
		if err := rows.Scan(&e.Name, &e.Version, &e.Path); err != nil {
			return info, err
		}
		installed[e.Name] = e
	}
	if err := rows.Err(); err != nil {
		return info, err
	}

	for _, name := range slices.Concat(sessionExtensions, quackExtensions) {
		if os.Getenv(ExtensionDirEnv) != "" && slices.Contains(sessionExtensions, name) {
			path, err := ExtensionPath(name)
			if err != nil {
				path = ""
			}
			info.Extensions = append(info.Extensions, ExtensionVersion{Name: name, Path: path})
			continue
		}
		e, ok := installed[name]
		if !ok {
			e = ExtensionVersion{Name: name}
		}
		info.Extensions = append(info.Extensions, e)
	}
	return info, nil
}
