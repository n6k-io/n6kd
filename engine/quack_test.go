package engine_test

import (
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/n6k-io/n6kd/engine"
	"github.com/n6k-io/n6kd/perms"
)

const quackSchemaSQL = `
ATTACH ':memory:' AS app;
CREATE TABLE app.main.users (id INTEGER PRIMARY KEY, tenant_id INTEGER, name VARCHAR);
INSERT INTO app.main.users VALUES (1, 42, 'ada'), (2, 42, 'grace'), (3, 7, 'linus');
CREATE TABLE app.main.secrets (k VARCHAR);
`

const quackPermsYAML = `
catalogs:
  app:
    tables:
      main.users:
        primary_key: [id]
        select: "tenant_id = 42"
        insert: "tenant_id = 42"
`

const quackToken = "test-token"

var (
	quackHomeOnce sync.Once
	quackHome     string
	quackHomeErr  error
)

// quackExtensionHome is one extension directory for the whole run, holding
// quack and httpfs: ServeQuack loads them, and vcatbridge, by name from there.
func quackExtensionHome(t *testing.T) string {
	t.Helper()
	quackHomeOnce.Do(func() {
		quackHome, quackHomeErr = os.MkdirTemp("", "n6kd-quack-ext-")
		if quackHomeErr != nil {
			return
		}
		db, err := sql.Open("duckdb", "?extension_directory="+url.QueryEscape(quackHome))
		if err != nil {
			quackHomeErr = err
			return
		}
		defer func() { _ = db.Close() }()
		for _, name := range []string{"quack", "httpfs"} {
			if _, err := db.Exec("INSTALL " + name); err != nil {
				quackHomeErr = fmt.Errorf("install %s: %w", name, err)
				return
			}
		}
	})
	if quackHomeErr != nil {
		t.Fatalf("quack extension home: %v", quackHomeErr)
	}
	return quackHome
}

// serveQuack opens a quack engine and serves it on a free localhost port,
// returning the client's URI.
func serveQuack(t *testing.T, schemaSQL, permsYML string, lockdown bool) string {
	t.Helper()
	uri, _ := serveQuackWithToken(t, schemaSQL, permsYML, lockdown, quackToken)
	return uri
}

func serveQuackWithToken(t *testing.T, schemaSQL, permsYML string, lockdown bool, token string) (uri, authToken string) {
	t.Helper()
	if os.Getenv(engine.ExtensionDirEnv) == "" {
		t.Skipf("%s not set", engine.ExtensionDirEnv)
	}
	t.Setenv(engine.ExtensionHomeEnv, quackExtensionHome(t))

	opts := engine.Options{Boot: []engine.Step{{SQL: schemaSQL}}, Lockdown: lockdown, Quack: true}
	if permsYML != "" {
		rules, err := perms.Parse([]byte(permsYML))
		if err != nil {
			t.Fatal(err)
		}
		opts.Perms = rules
	}
	eng, err := engine.Open(t.Context(), opts, zaptest.NewLogger(t).Sugar())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() {
		if err := eng.Close(); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})

	uri = "quack:localhost:" + freePort(t)
	_, authToken, err = eng.ServeQuack(t.Context(), uri, token, false)
	if err != nil {
		t.Fatalf("serve quack: %v", err)
	}
	return uri, authToken
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
}

// quackClient is a separate DuckDB attached to uri as app, presenting token.
func quackClient(t *testing.T, uri, token string) (*sql.DB, error) {
	t.Helper()
	db, err := sql.Open("duckdb", "?extension_directory="+url.QueryEscape(quackExtensionHome(t)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{
		"LOAD quack",
		fmt.Sprintf("CREATE SECRET (TYPE quack, TOKEN '%s')", token),
		fmt.Sprintf("ATTACH '%s' AS app", uri),
	} {
		if _, err := db.ExecContext(t.Context(), stmt); err != nil {
			return nil, fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return db, nil
}

func mustQuackClient(t *testing.T, uri string) *sql.DB {
	t.Helper()
	db, err := quackClient(t, uri, quackToken)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// quack's ATTACH cannot run UPDATE or DELETE; quack_query sends them as they are.
func remote(t *testing.T, db *sql.DB, uri, stmt string) error {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "FROM quack_query(?, ?)", uri, stmt)
	if err != nil {
		return err
	}
	return rows.Close()
}

func TestQuackServesBridgedCatalog(t *testing.T) {
	uri := serveQuack(t, quackSchemaSQL, quackPermsYAML, false)
	db := mustQuackClient(t, uri)

	ids := func() string {
		var got string
		if err := db.QueryRowContext(t.Context(),
			`SELECT string_agg(id::VARCHAR, ',' ORDER BY id) FROM app.users`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := ids(); got != "1,2" {
		t.Errorf("select: got ids %s, want 1,2", got)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO app.users VALUES (4, 42, 'barbara')`); err != nil {
		t.Errorf("insert passing check: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO app.users VALUES (5, 7, 'mallory')`); err == nil {
		t.Error("insert violating check succeeded")
	}
	if err := remote(t, db, uri, `DELETE FROM users WHERE id = 2`); err == nil {
		t.Error("delete without a grant succeeded")
	}
	if got := ids(); got != "1,2,4" {
		t.Errorf("after writes: got ids %s, want 1,2,4", got)
	}
	if _, err := db.ExecContext(t.Context(), `FROM app.secrets`); err == nil {
		t.Error("unlisted table was reachable")
	}
}

func TestQuackRefusesWrongToken(t *testing.T) {
	uri := serveQuack(t, quackSchemaSQL, quackPermsYAML, false)
	if _, err := quackClient(t, uri, "wrong-token"); err == nil || !strings.Contains(err.Error(), "Authentication failed") {
		t.Fatalf("attach with the wrong token: %v, want authentication refused", err)
	}
}

// The generated token must not keep lockdown from applying after it.
func TestQuackGeneratesTokenUnderLockdown(t *testing.T) {
	uri, authToken := serveQuackWithToken(t, quackSchemaSQL, quackPermsYAML, true, "")
	if authToken == "" {
		t.Fatal("no token generated")
	}
	if _, err := quackClient(t, uri, authToken); err != nil {
		t.Fatalf("attach with the generated token: %v", err)
	}
}

func TestQuackLockdownBlocksFileAccess(t *testing.T) {
	uri := serveQuack(t, quackSchemaSQL, quackPermsYAML, true)
	db := mustQuackClient(t, uri)

	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM app.users`).Scan(&count); err != nil {
		t.Fatalf("read under lockdown: %v", err)
	}

	path := filepath.Join(t.TempDir(), "probe.csv")
	if err := os.WriteFile(path, []byte("a\n1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := remote(t, db, uri, fmt.Sprintf(`SELECT * FROM read_csv('%s')`, path))
	if err == nil || !strings.Contains(err.Error(), "disabled by configuration") {
		t.Errorf("read_csv under lockdown: %v, want the filesystem disabled", err)
	}
	if err := remote(t, db, uri, fmt.Sprintf(`CALL quack_stop('%s')`, uri)); err == nil || !strings.Contains(err.Error(), "not in the catalog") {
		t.Errorf("quack_stop under lockdown: %v, want it dropped", err)
	}
}

func TestQuackDirectModeServesSource(t *testing.T) {
	uri := serveQuack(t, schemaSQL, "", false)
	db := mustQuackClient(t, uri)

	if _, err := db.ExecContext(t.Context(), `INSERT INTO app.audit_log VALUES (2, 'logout')`); err != nil {
		t.Fatalf("insert without perms: %v", err)
	}
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM app.audit_log`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("audit_log: got %d rows, want 2", count)
	}
}

func TestQuackRefusesSeveralCatalogs(t *testing.T) {
	rules, err := perms.Parse([]byte(`
catalogs:
  app:
    tables:
      main.users: { select: true }
  other:
    tables:
      main.t: { select: true }
`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.Open(t.Context(), engine.Options{Perms: rules, Quack: true}, zaptest.NewLogger(t).Sugar())
	if err == nil || !strings.Contains(err.Error(), "one catalog") {
		t.Fatalf("open with two catalogs: %v, want refused", err)
	}
}

// A quack client sees only the main database, so tables the boot SQL left
// anywhere else must stop the start rather than go missing.
func TestQuackDirectModeRefusesTablesOutsideMain(t *testing.T) {
	if os.Getenv(engine.ExtensionDirEnv) == "" {
		t.Skipf("%s not set", engine.ExtensionDirEnv)
	}
	t.Setenv(engine.ExtensionHomeEnv, quackExtensionHome(t))

	_, err := engine.Open(t.Context(), engine.Options{
		Boot:  []engine.Step{{SQL: bridgedSchemaSQL}},
		Quack: true,
	}, zaptest.NewLogger(t).Sugar())
	if err == nil || !strings.Contains(err.Error(), `tables in [app]`) {
		t.Fatalf("open with tables in app: %v, want refused", err)
	}
}
