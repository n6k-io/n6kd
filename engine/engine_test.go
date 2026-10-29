package engine_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/n6k-io/n6kd/engine"
	"github.com/n6k-io/n6kd/perms"
)

const schemaSQL = `
CREATE TABLE users (id INTEGER PRIMARY KEY, name VARCHAR);
INSERT INTO users VALUES (1, 'Ada'), (2, 'Grace');
CREATE TABLE audit_log (id INTEGER PRIMARY KEY, action VARCHAR);
INSERT INTO audit_log VALUES (1, 'login');
`

// Bridged mode needs a catalog of its own: memory cannot be served.
const bridgedSchemaSQL = `
ATTACH ':memory:' AS app;
CREATE TABLE app.main.users (id INTEGER PRIMARY KEY, name VARCHAR);
INSERT INTO app.main.users VALUES (1, 'Ada'), (2, 'Grace');
CREATE TABLE app.main.audit_log (id INTEGER PRIMARY KEY, action VARCHAR);
INSERT INTO app.main.audit_log VALUES (1, 'login');
`

const permsYAML = `
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

// open builds an engine from the fixtures above, skipping when the extensions
// have not been built — this needs the real vcatbridge, not a stand-in.
func open(t *testing.T) *engine.Engine {
	t.Helper()
	return openWith(t, bridgedSchemaSQL, permsYAML)
}

func openWith(t *testing.T, schemaSQL, permsYML string) *engine.Engine {
	t.Helper()
	if os.Getenv(engine.ExtensionDirEnv) == "" {
		t.Skipf("%s not set", engine.ExtensionDirEnv)
	}

	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	permsPath := filepath.Join(dir, "perms.yml")
	if err := os.WriteFile(schemaPath, []byte(schemaSQL), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(permsPath, []byte(permsYML), 0o600); err != nil {
		t.Fatal(err)
	}

	rules, err := perms.Load(permsPath)
	if err != nil {
		t.Fatalf("load perms: %v", err)
	}
	eng, err := engine.Open(t.Context(), engine.Options{Boot: []engine.Step{{Path: schemaPath}}, Perms: rules}, zaptest.NewLogger(t).Sugar())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() {
		if err := eng.Close(); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})
	return eng
}

func newSession(t *testing.T, eng *engine.Engine) *engine.Session {
	t.Helper()
	session, err := eng.NewSession(t.Context(), zaptest.NewLogger(t).Sugar())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close session: %v", err)
		}
	})
	return session
}

// A session reads rows that live in the source database, not in its own.
func TestSessionReadsBridgedRows(t *testing.T) {
	session := newSession(t, open(t))

	var count int
	if err := session.DB().QueryRowContext(t.Context(),
		`SELECT count(*) FROM app.main.users`).Scan(&count); err != nil {
		t.Fatalf("query bridged table: %v", err)
	}
	if count != 2 {
		t.Errorf("app.main.users: got %d rows, want 2", count)
	}
}

func TestReadWriteTableAcceptsInsert(t *testing.T) {
	session := newSession(t, open(t))

	if _, err := session.DB().ExecContext(t.Context(),
		`INSERT INTO app.main.users VALUES (3, 'Katherine')`); err != nil {
		t.Fatalf("insert into writable table: %v", err)
	}

	var count int
	if err := session.DB().QueryRowContext(t.Context(),
		`SELECT count(*) FROM app.main.users`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("after insert: got %d rows, want 3", count)
	}
}

// audit_log grants only select in the fixture perms, so an INSERT through the bridge fails.
func TestReadOnlyTableRejectsInsert(t *testing.T) {
	session := newSession(t, open(t))

	_, err := session.DB().ExecContext(t.Context(),
		`INSERT INTO app.main.audit_log VALUES (2, 'logout')`)
	if err == nil {
		t.Fatal("insert into read-only table succeeded, want a permission error")
	}
	t.Logf("rejected as expected: %v", err)
}

// A table the schema created but perms.yml never named must not be reachable.
func TestUnlistedTableIsNotBridged(t *testing.T) {
	session := newSession(t, open(t))

	var count int
	err := session.DB().QueryRowContext(t.Context(),
		`SELECT count(*) FROM app.main.secrets`).Scan(&count)
	if err == nil {
		t.Fatal("unlisted table was reachable")
	}
}

func openDirect(t *testing.T) *engine.Engine {
	t.Helper()
	if os.Getenv(engine.ExtensionDirEnv) == "" {
		t.Skipf("%s not set", engine.ExtensionDirEnv)
	}

	schemaPath := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(schemaSQL), 0o600); err != nil {
		t.Fatal(err)
	}
	eng, err := engine.Open(t.Context(), engine.Options{Boot: []engine.Step{{Path: schemaPath}}}, zaptest.NewLogger(t).Sugar())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() {
		if err := eng.Close(); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})
	return eng
}

func TestDirectModeServesEverySourceTable(t *testing.T) {
	eng := openDirect(t)
	if got := eng.Catalogs(); !slices.Contains(got, "memory") {
		t.Fatalf("catalogs = %v, want the source catalog", got)
	}
	session := newSession(t, eng)

	// audit_log is the table perms.yml would have made read-only.
	for _, table := range []string{"users", "audit_log"} {
		var count int
		if err := session.DB().QueryRowContext(t.Context(),
			`SELECT count(*) FROM memory.main.`+table).Scan(&count); err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
	}
	if _, err := session.DB().ExecContext(t.Context(),
		`INSERT INTO memory.main.audit_log VALUES (2, 'logout')`); err != nil {
		t.Fatalf("insert without perms: %v", err)
	}
}

func TestDirectModeSessionCloseLeavesSourceOpen(t *testing.T) {
	eng := openDirect(t)

	session, err := eng.NewSession(t.Context(), zaptest.NewLogger(t).Sugar())
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}

	next := newSession(t, eng)
	var count int
	if err := next.DB().QueryRowContext(t.Context(),
		`SELECT count(*) FROM memory.main.users`).Scan(&count); err != nil {
		t.Fatalf("query after a session closed: %v", err)
	}
	if count != 2 {
		t.Errorf("memory.main.users: got %d rows, want 2", count)
	}
}

// Lockdown has to survive the bridge: the session still reads its catalogs, but
// the filesystem is gone and cannot be turned back on.
func TestLockdownBlocksFileAccessInBridgedSession(t *testing.T) {
	if os.Getenv(engine.ExtensionDirEnv) == "" {
		t.Skipf("%s not set", engine.ExtensionDirEnv)
	}

	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	permsPath := filepath.Join(dir, "perms.yml")
	if err := os.WriteFile(schemaPath, []byte(bridgedSchemaSQL), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(permsPath, []byte(permsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err := perms.Load(permsPath)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.Open(t.Context(),
		engine.Options{Boot: []engine.Step{{Path: schemaPath}}, Perms: rules, Lockdown: true}, zaptest.NewLogger(t).Sugar())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() {
		if err := eng.Close(); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})
	session := newSession(t, eng)

	var count int
	if err := session.DB().QueryRowContext(t.Context(),
		`SELECT count(*) FROM app.main.users`).Scan(&count); err != nil {
		t.Fatalf("bridged read under lockdown: %v", err)
	}
	if count != 2 {
		t.Errorf("app.main.users: got %d rows, want 2", count)
	}

	if _, err := session.DB().QueryContext(t.Context(),
		`SELECT * FROM read_csv('`+permsPath+`')`); err == nil {
		t.Error("read_csv reached the filesystem under lockdown")
	}
	// enable_external_access is refused on any running database, locked or not,
	// so lock_configuration is what this message distinguishes.
	_, err = session.DB().ExecContext(t.Context(), `SET enable_external_access=true`)
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Errorf("lockdown was reversible: %v", err)
	}
}

func TestLockdownBlocksFileAccessInDirectMode(t *testing.T) {
	if os.Getenv(engine.ExtensionDirEnv) == "" {
		t.Skipf("%s not set", engine.ExtensionDirEnv)
	}

	schemaPath := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(schemaSQL), 0o600); err != nil {
		t.Fatal(err)
	}
	eng, err := engine.Open(t.Context(),
		engine.Options{Boot: []engine.Step{{Path: schemaPath}}, Lockdown: true}, zaptest.NewLogger(t).Sugar())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() {
		if err := eng.Close(); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})
	session := newSession(t, eng)

	var count int
	if err := session.DB().QueryRowContext(t.Context(),
		`SELECT count(*) FROM memory.main.users`).Scan(&count); err != nil {
		t.Fatalf("direct read under lockdown: %v", err)
	}
	if _, err := session.DB().QueryContext(t.Context(),
		`SELECT * FROM read_csv('`+schemaPath+`')`); err == nil {
		t.Error("read_csv reached the filesystem under lockdown")
	}
}

// The control for the lockdown tests: with Lockdown false, file access works.
func TestWithoutLockdownFileAccessStillWorks(t *testing.T) {
	csv := filepath.Join(t.TempDir(), "rows.csv")
	if err := os.WriteFile(csv, []byte("id,name\n1,Ada\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := newSession(t, open(t))

	var count int
	if err := session.DB().QueryRowContext(t.Context(),
		`SELECT count(*) FROM read_csv('`+csv+`')`).Scan(&count); err != nil {
		t.Errorf("file access was restricted without lockdown: %v", err)
	}
}

// openFails is the negative half of open: a schema and perms.yml that must not
// boot.
func openFails(t *testing.T, schema, permsYML string) error {
	t.Helper()
	if os.Getenv(engine.ExtensionDirEnv) == "" {
		t.Skipf("%s not set", engine.ExtensionDirEnv)
	}

	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(schema), 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err := perms.Parse([]byte(permsYML))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.Open(t.Context(), engine.Options{Boot: []engine.Step{{Path: schemaPath}}, Perms: rules}, zaptest.NewLogger(t).Sugar())
	if err == nil {
		_ = eng.Close()
		t.Fatal("engine opened despite an invalid configuration")
	}
	return err
}

func TestMissingTableFailsAtStartup(t *testing.T) {
	// The schema omits audit_log, which perms.yml still names.
	err := openFails(t, "ATTACH ':memory:' AS app;\nCREATE TABLE app.main.users (id INTEGER PRIMARY KEY, name VARCHAR);", permsYAML)
	if !strings.Contains(err.Error(), "main.audit_log") {
		t.Errorf("got %v, want an error naming main.audit_log", err)
	}
}

func TestWritableTableWithoutPrimaryKeyRefusesWrites(t *testing.T) {
	const schema = `ATTACH ':memory:' AS app; CREATE TABLE app.main.events (id INTEGER, action VARCHAR);`
	const rules = `
catalogs:
  app:
    tables:
      main.events:
        select: true
        insert: true
        update: true
        delete: true
`
	db := newSession(t, openWith(t, schema, rules)).DB()
	for _, stmt := range []string{
		`UPDATE app.main.events SET action = 'x' WHERE id = 1`,
		`DELETE FROM app.main.events WHERE id = 1`,
	} {
		if _, err := db.ExecContext(t.Context(), stmt); err == nil || !strings.Contains(err.Error(), "has no key") {
			t.Errorf("%s: got %v, want a missing key error", stmt, err)
		}
	}
}

func TestInsertOnlyTableNeedsNoPrimaryKey(t *testing.T) {
	session := newSession(t, openWith(t,
		`ATTACH ':memory:' AS app; CREATE TABLE app.main.events (id INTEGER, action VARCHAR);`, `
catalogs:
  app:
    tables:
      main.events:
        insert: true
`))
	if _, err := session.DB().ExecContext(t.Context(),
		`INSERT INTO app.main.events VALUES (1, 'login')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := session.DB().ExecContext(t.Context(),
		`SELECT count(*) FROM app.main.events`); err == nil {
		t.Error("select succeeded without a select grant")
	}
}

func TestRowPoliciesAreEnforced(t *testing.T) {
	session := newSession(t, openWith(t, `
ATTACH ':memory:' AS app;
CREATE TABLE app.main.orders (id INTEGER PRIMARY KEY, status VARCHAR, amount INTEGER);
INSERT INTO app.main.orders VALUES (1, 'open', 10), (2, 'closed', 20), (3, 'deleted', 30);
`, `
catalogs:
  app:
    tables:
      main.orders:
        select: "status <> 'deleted'"
        insert: "amount > 0"
        update: { using: "status = 'open'", check: "amount > 0" }
        delete: "status = 'closed'"
`))
	db := session.DB()
	exec := func(stmt string) (int64, error) {
		res, err := db.ExecContext(t.Context(), stmt)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	ids := func() string {
		var got string
		if err := db.QueryRowContext(t.Context(),
			`SELECT string_agg(id::VARCHAR, ',' ORDER BY id) FROM app.main.orders`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	if got := ids(); got != "1,2" {
		t.Errorf("select using: got ids %s, want 1,2", got)
	}
	if _, err := exec(`INSERT INTO app.main.orders VALUES (4, 'open', -1)`); err == nil {
		t.Error("insert violating check succeeded")
	}
	if _, err := exec(`INSERT INTO app.main.orders VALUES (4, 'open', 5)`); err != nil {
		t.Errorf("insert passing check: %v", err)
	}
	if n, err := exec(`UPDATE app.main.orders SET amount = 99`); err != nil || n != 2 {
		t.Errorf("update using: got %d rows, %v; want the 2 open rows", n, err)
	}
	if _, err := exec(`UPDATE app.main.orders SET amount = -1 WHERE id = 1`); err == nil {
		t.Error("update violating check succeeded")
	}
	if n, err := exec(`DELETE FROM app.main.orders`); err != nil || n != 1 {
		t.Errorf("delete using: got %d rows, %v; want the 1 closed row", n, err)
	}
	if got := ids(); got != "1,4" {
		t.Errorf("after writes: got ids %s, want 1,4", got)
	}
}

func TestContextValuesReachPolicies(t *testing.T) {
	eng := openWith(t, `
ATTACH ':memory:' AS app;
CREATE TABLE app.main.orders (id INTEGER PRIMARY KEY, owner VARCHAR);
INSERT INTO app.main.orders VALUES (1, 'alice'), (2, 'bob'), (3, 'alice');
`, `
catalogs:
  app:
    tables:
      main.orders:
        select: "owner = $sub"
`)
	log := zaptest.NewLogger(t).Sugar()
	session, err := eng.OpenSession(t.Context(), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close session: %v", err)
		}
	})
	if err := eng.Admit(t.Context(), session, map[string]any{"sub": "alice"}, log); err != nil {
		t.Fatalf("admit: %v", err)
	}

	var got string
	if err := session.DB().QueryRowContext(t.Context(),
		`SELECT string_agg(id::VARCHAR, ',' ORDER BY id) FROM app.main.orders`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "1,3" {
		t.Errorf("got ids %s, want 1,3", got)
	}
}

func TestPolicyWithUnsetContextFailsToAdmit(t *testing.T) {
	eng := openWith(t, `
ATTACH ':memory:' AS app;
CREATE TABLE app.main.orders (id INTEGER PRIMARY KEY, owner VARCHAR);
`, `
catalogs:
  app:
    tables:
      main.orders:
        select: "owner = $sub"
`)
	if _, err := eng.NewSession(t.Context(), zaptest.NewLogger(t).Sugar()); err == nil || !strings.Contains(err.Error(), "$sub") {
		t.Errorf("got %v, want an error naming $sub", err)
	}
}

func TestPrimaryKeyOverrideWithUnknownColumnFailsAtStartup(t *testing.T) {
	const schema = `ATTACH ':memory:' AS app; CREATE TABLE app.main.events (id INTEGER, action VARCHAR);`
	const rules = `
catalogs:
  app:
    tables:
      main.events:
        primary_key: [event_id]
        select: true
        insert: true
        update: true
        delete: true
`
	err := openFails(t, schema, rules)
	if !strings.Contains(err.Error(), "event_id") {
		t.Errorf("got %v, want an unknown column error naming event_id", err)
	}
}

// A writable table whose key is declared in the schema needs no override.
func TestDiscoveredPrimaryKeyNeedsNoOverride(t *testing.T) {
	newSession(t, openWith(t,
		"ATTACH ':memory:' AS app; CREATE TABLE app.main.events (id INTEGER PRIMARY KEY, action VARCHAR);", `
catalogs:
  app:
    tables:
      main.events:
        select: true
        insert: true
        update: true
        delete: true
`))
}

func TestBridgeSpansSchemasInOneCatalog(t *testing.T) {
	if os.Getenv(engine.ExtensionDirEnv) == "" {
		t.Skipf("%s not set", engine.ExtensionDirEnv)
	}

	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	permsPath := filepath.Join(dir, "perms.yml")
	if err := os.WriteFile(schemaPath, []byte(`
ATTACH ':memory:' AS app;
CREATE TABLE app.main.users (id INTEGER PRIMARY KEY, name VARCHAR);
INSERT INTO app.main.users VALUES (1, 'Ada');
CREATE SCHEMA app.sales;
CREATE TABLE app.sales.orders (id INTEGER PRIMARY KEY, total INTEGER);
INSERT INTO app.sales.orders VALUES (1, 10), (2, 20);
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(permsPath, []byte(`
catalogs:
  app:
    tables:
      main.users:
        select: true
      sales.orders:
        select: true
        insert: true
        update: true
        delete: true
`), 0o600); err != nil {
		t.Fatal(err)
	}

	rules, err := perms.Load(permsPath)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.Open(t.Context(), engine.Options{Boot: []engine.Step{{Path: schemaPath}}, Perms: rules}, zaptest.NewLogger(t).Sugar())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() {
		if err := eng.Close(); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})
	session := newSession(t, eng)

	for query, want := range map[string]int{
		`SELECT count(*) FROM app.main.users`:   1,
		`SELECT count(*) FROM app.sales.orders`: 2,
	} {
		var got int
		if err := session.DB().QueryRowContext(t.Context(), query).Scan(&got); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if got != want {
			t.Errorf("%s: got %d, want %d", query, got, want)
		}
	}
	if _, err := session.DB().ExecContext(t.Context(), `INSERT INTO app.sales.orders VALUES (3, 30)`); err != nil {
		t.Fatalf("insert into writable table in second schema: %v", err)
	}
}
