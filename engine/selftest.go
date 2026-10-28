package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/perms"
)

const selftestSchema = `
ATTACH ':memory:' AS app;
CREATE TABLE app.main.users (id INTEGER PRIMARY KEY, name VARCHAR);
INSERT INTO app.main.users VALUES (1, 'Ada'), (2, 'Grace');
CREATE TABLE app.main.audit_log (id INTEGER PRIMARY KEY, action VARCHAR);
INSERT INTO app.main.audit_log VALUES (1, 'login');
`

const selftestPerms = `
catalogs:
  app:
    tables:
      main.users:
        primary_key: [id]
        select: true
        insert: true
        update: true
        delete: true
      main.audit_log:
        select: true
`

// SelfTest runs a whole engine against fixtures of its own: it loads the three
// extensions, builds a source database, bridges a session, and checks that the
// bridge both serves rows and refuses a write it should refuse.
//
// The Dockerfile runs this after its USER directive, so the baked extensions are
// checked as the account that serves rather than as root.
func SelfTest(ctx context.Context, log *zap.SugaredLogger) error {
	dir, err := os.MkdirTemp("", "n6kd-selftest")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	schemaPath := filepath.Join(dir, "schema.sql")
	permsPath := filepath.Join(dir, "perms.yml")
	if err := os.WriteFile(schemaPath, []byte(selftestSchema), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(permsPath, []byte(selftestPerms), 0o600); err != nil {
		return err
	}

	rules, err := perms.Load(permsPath)
	if err != nil {
		return fmt.Errorf("selftest perms: %w", err)
	}

	eng, err := Open(ctx, Options{Boot: []Step{{Path: schemaPath}}, Perms: rules, Lockdown: true}, log)
	if err != nil {
		return fmt.Errorf("selftest engine: %w", err)
	}
	defer func() { _ = eng.Close() }()
	log.Infof("selftest: extensions loaded and source schema built")

	session, err := eng.NewSession(ctx, log)
	if err != nil {
		return fmt.Errorf("selftest session: %w", err)
	}
	defer func() { _ = session.Close() }()

	var count int
	if err := session.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM app.main.users`).Scan(&count); err != nil {
		return fmt.Errorf("selftest read through bridge: %w", err)
	}
	if count != 2 {
		return fmt.Errorf("selftest read through bridge: got %d rows, want 2", count)
	}
	log.Infof("selftest: bridged read returned %d rows", count)

	if _, err := session.DB().ExecContext(ctx,
		`INSERT INTO app.main.users VALUES (3, 'Katherine')`); err != nil {
		return fmt.Errorf("selftest write to writable table: %w", err)
	}
	log.Infof("selftest: write to a writable table accepted")

	if _, err := session.DB().ExecContext(ctx,
		`INSERT INTO app.main.audit_log VALUES (2, 'logout')`); err == nil {
		return fmt.Errorf("selftest: write to a read-only table was accepted")
	}
	log.Infof("selftest: write to a read-only table refused")

	return nil
}
