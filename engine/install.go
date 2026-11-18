package engine

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"

	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/internal/sqlquote"
)

const ExtensionRepoEnv = "N6K_EXTENSION_REPO"

const defaultExtensionRepo = "https://storage.googleapis.com/n6k-duckdb-release"

type InstallOptions struct {
	// Force re-downloads extensions DuckDB already has; without it INSTALL skips
	// them, even when Versions asks for another version.
	Force bool
	// Versions maps a session extension to a release-bucket version or tag; ""
	// or absent installs the unversioned build.
	Versions map[string]string
}

// VersionedExtensions are the extensions InstallOptions.Versions can pin.
func VersionedExtensions() []string {
	return slices.Clone(sessionExtensions)
}

// InstallExtensions downloads every extension n6kd or quack loads into DuckDB's
// extension_directory.
func InstallExtensions(ctx context.Context, log *zap.SugaredLogger, opts InstallOptions) error {
	db, err := sql.Open("duckdb", extensionDSN())
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	repo := os.Getenv(ExtensionRepoEnv)
	if repo == "" {
		repo = defaultExtensionRepo
	}
	install := func(name, repo, version string) error {
		// #nosec G701 -- installStatement quotes every part with sqlquote.String.
		if _, err := db.ExecContext(ctx, installStatement(name, repo, version, opts.Force)); err != nil {
			return fmt.Errorf("install %s: %w", name, err)
		}
		var installed string
		row := db.QueryRowContext(ctx, "SELECT coalesce(extension_version, '') FROM duckdb_extensions() WHERE extension_name = ?", name)
		if err := row.Scan(&installed); err != nil {
			return fmt.Errorf("install %s: read installed version: %w", name, err)
		}
		if !versionMatches(installed, version) {
			return fmt.Errorf("install %s: %s is installed but the config pins %s; run n6kd install without --skip-installed", name, installed, version)
		}
		log.Infof("extension %s %s (from %s, re-downloaded %v)", name, installed, cmp.Or(repo, "duckdb"), opts.Force)
		return nil
	}
	for _, name := range sessionExtensions {
		if err := install(name, repo, opts.Versions[name]); err != nil {
			return err
		}
	}
	for _, name := range quackExtensions {
		if err := install(name, "", ""); err != nil {
			return err
		}
	}
	return nil
}

// versionMatches treats a pin as a tag over dot-separated versions, so v0 and
// v0.2 both match v0.2.5.
func versionMatches(installed, pin string) bool {
	return pin == "" || installed == pin || strings.HasPrefix(installed, pin+".")
}

// installStatement leaves out FROM for DuckDB's own repository.
func installStatement(name, repo, version string, force bool) string {
	var b strings.Builder
	if force {
		b.WriteString("FORCE ")
	}
	b.WriteString("INSTALL " + sqlquote.String(name))
	if repo != "" {
		b.WriteString(" FROM " + sqlquote.String(repo))
	}
	if version != "" {
		b.WriteString(" VERSION " + sqlquote.String(version))
	}
	return b.String()
}
