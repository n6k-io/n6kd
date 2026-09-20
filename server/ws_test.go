package server

import (
	"database/sql"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
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
expose:
  app.users: readwrite
  app.audit_log: read
keys:
  app.users: [id]
`

// newTestServer serves the fixtures above over the real /ws route.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	extDir := os.Getenv(engine.ExtensionDirEnv)
	if extDir == "" {
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
	log := zaptest.NewLogger(t).Sugar()
	eng, err := engine.Open(t.Context(), engine.Options{Boot: []engine.Step{{Path: schemaPath}}, Perms: rules}, log)
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() {
		if err := eng.Close(); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})

	srv := httptest.NewServer(newRouter(Config{}, eng, log))
	t.Cleanup(srv.Close)
	return srv
}

func loadClientExtension(t *testing.T, db *sql.DB) {
	t.Helper()
	path, err := engine.ExtensionPath("n6k_client")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), fmt.Sprintf("LOAD '%s'", path)); err != nil {
		t.Fatalf("load n6k client extension: %v", err)
	}
}

func newDirectTestServer(t *testing.T) *httptest.Server {
	return newDirectTestServerWithKey(t, "")
}

func newDirectTestServerWithKey(t *testing.T, apiKey string) *httptest.Server {
	t.Helper()
	if os.Getenv(engine.ExtensionDirEnv) == "" {
		t.Skipf("%s not set", engine.ExtensionDirEnv)
	}

	schemaPath := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(schemaSQL), 0o600); err != nil {
		t.Fatal(err)
	}
	log := zaptest.NewLogger(t).Sugar()
	eng, err := engine.Open(t.Context(),
		engine.Options{Boot: []engine.Step{{Path: schemaPath}}, APIKey: apiKey}, log)
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() {
		if err := eng.Close(); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})

	srv := httptest.NewServer(newRouter(Config{}, eng, log))
	t.Cleanup(srv.Close)
	return srv
}

func TestDirectModeOverWebSocket(t *testing.T) {
	db := newClientAs(t, newDirectTestServer(t), "remote", "memory")

	var count int
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM remote.main.users`).Scan(&count); err != nil {
		t.Fatalf("query over ws: %v", err)
	}
	if count != 2 {
		t.Errorf("remote.main.users: got %d rows, want 2", count)
	}

	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO remote.main.audit_log VALUES (2, 'logout')`); err != nil {
		t.Fatalf("insert into an unpermissioned table over ws: %v", err)
	}
}

func newAuthTestServer(t *testing.T, apiKey string) *httptest.Server {
	t.Helper()
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
	log := zaptest.NewLogger(t).Sugar()
	eng, err := engine.Open(t.Context(),
		engine.Options{Boot: []engine.Step{{Path: schemaPath}}, Perms: rules, APIKey: apiKey}, log)
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() {
		if err := eng.Close(); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})

	srv := httptest.NewServer(newRouter(Config{}, eng, log))
	t.Cleanup(srv.Close)
	return srv
}

// attachWithToken is the client side of --api-key: the token rides the ATTACH,
// and the server either takes it or refuses the connection.
func attachWithToken(t *testing.T, srv *httptest.Server, token string) error {
	t.Helper()
	db, err := sql.Open("duckdb", "?allow_unsigned_extensions=true")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	loadClientExtension(t, db)

	authority := strings.TrimPrefix(srv.URL, "http://")
	attach := fmt.Sprintf("ATTACH 'n6k://%s' AS app (TYPE n6k, TOKEN '%s')", authority, token)
	if token == "" {
		attach = fmt.Sprintf("ATTACH 'n6k://%s' AS app (TYPE n6k, ANONYMOUS)", authority)
	}
	if _, err := db.ExecContext(t.Context(), attach); err != nil {
		return err
	}
	var count int
	return db.QueryRowContext(t.Context(), `SELECT count(*) FROM app.main.users`).Scan(&count)
}

func TestAPIKeyAcceptsMatchingToken(t *testing.T) {
	if err := attachWithToken(t, newAuthTestServer(t, "s3cret"), "s3cret"); err != nil {
		t.Fatalf("attach with the right token: %v", err)
	}
}

func TestAPIKeyRejectsWrongToken(t *testing.T) {
	err := attachWithToken(t, newAuthTestServer(t, "s3cret"), "wrong")
	if err == nil {
		t.Fatal("attach with the wrong token succeeded")
	}
	t.Logf("rejected as expected: %v", err)
}

// Direct mode registers the verifier on the shared source rather than on a
// per-session database, so it needs its own coverage.
func TestAPIKeyInDirectMode(t *testing.T) {
	srv := newDirectTestServerWithKey(t, "s3cret")

	authority := strings.TrimPrefix(srv.URL, "http://")
	for _, test := range []struct {
		name    string
		options string
		wantErr bool
	}{
		{"right token", "TYPE n6k, CATALOG 'memory', TOKEN 's3cret'", false},
		{"wrong token", "TYPE n6k, CATALOG 'memory', TOKEN 'wrong'", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, err := sql.Open("duckdb", "?allow_unsigned_extensions=true")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()

			loadClientExtension(t, db)

			var count int
			if _, attachErr := db.ExecContext(t.Context(),
				fmt.Sprintf("ATTACH 'n6k://%s' AS remote (%s)", authority, test.options)); attachErr != nil {
				err = attachErr
			} else {
				err = db.QueryRowContext(t.Context(), `SELECT count(*) FROM remote.main.users`).Scan(&count)
			}

			if test.wantErr && err == nil {
				t.Fatal("connected despite a bad token")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("connect with the right token: %v", err)
			}
		})
	}
}

func TestAPIKeyRejectsMissingToken(t *testing.T) {
	err := attachWithToken(t, newAuthTestServer(t, "s3cret"), "")
	if err == nil {
		t.Fatal("attach with no token succeeded")
	}
	t.Logf("rejected as expected: %v", err)
}

// With lockdown on, a client's passthrough SQL cannot read the server's disk.
func TestLockdownBlocksClientFileAccessOverWebSocket(t *testing.T) {
	if os.Getenv(engine.ExtensionDirEnv) == "" {
		t.Skipf("%s not set", engine.ExtensionDirEnv)
	}

	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "schema.sql")
	permsPath := filepath.Join(dir, "perms.yml")
	secret := filepath.Join(dir, "secret.csv")
	if err := os.WriteFile(schemaPath, []byte(bridgedSchemaSQL), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(permsPath, []byte(permsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte("id,value\n1,classified\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rules, err := perms.Load(permsPath)
	if err != nil {
		t.Fatal(err)
	}
	log := zaptest.NewLogger(t).Sugar()
	eng, err := engine.Open(t.Context(),
		engine.Options{Boot: []engine.Step{{Path: schemaPath}}, Perms: rules, Lockdown: true}, log)
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() {
		if err := eng.Close(); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})
	srv := httptest.NewServer(newRouter(Config{}, eng, log))
	t.Cleanup(srv.Close)

	db := newClient(t, srv)

	var count int
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM app.main.users`).Scan(&count); err != nil {
		t.Fatalf("bridged read under lockdown: %v", err)
	}
	if count != 2 {
		t.Errorf("app.main.users: got %d rows, want 2", count)
	}

	// n6k_catalog_query is the OP_QUERY passthrough: the SQL runs on the server,
	// unlike a bare read_csv, which the client would answer for itself.
	var value string
	err = db.QueryRowContext(t.Context(),
		fmt.Sprintf(`SELECT value FROM n6k_catalog_query('app', 'SELECT value FROM read_csv(''%s'')')`, secret),
	).Scan(&value)
	if err == nil {
		t.Fatalf("client read %q off the server's filesystem under lockdown", value)
	}
	t.Logf("refused as expected: %v", err)
}

// The control for the test above: without lockdown that same passthrough reads
// the server's disk.
func TestWithoutLockdownClientReachesServerFiles(t *testing.T) {
	srv := newTestServer(t)
	secret := filepath.Join(t.TempDir(), "secret.csv")
	if err := os.WriteFile(secret, []byte("id,value\n1,classified\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db := newClient(t, srv)

	var value string
	if err := db.QueryRowContext(t.Context(),
		fmt.Sprintf(`SELECT value FROM n6k_catalog_query('app', 'SELECT value FROM read_csv(''%s'')')`, secret),
	).Scan(&value); err != nil {
		t.Fatalf("passthrough read without lockdown: %v", err)
	}
	if value != "classified" {
		t.Errorf("got %q, want %q", value, "classified")
	}
}

// newClient is a plain DuckDB with the n6k client extension loaded, so these
// tests speak the real protocol rather than a hand-rolled stand-in.
func newClient(t *testing.T, srv *httptest.Server) *sql.DB {
	return newClientAs(t, srv, "app", "")
}

func newClientAs(t *testing.T, srv *httptest.Server, alias, catalog string) *sql.DB {
	t.Helper()
	db, err := sql.Open("duckdb", "?allow_unsigned_extensions=true")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close client: %v", err)
		}
	})

	loadClientExtension(t, db)

	authority := strings.TrimPrefix(srv.URL, "http://")
	attach := fmt.Sprintf("ATTACH 'n6k://%s' AS %s (TYPE n6k)", authority, alias)
	if catalog != "" {
		attach = fmt.Sprintf("ATTACH 'n6k://%s' AS %s (TYPE n6k, CATALOG '%s')", authority, alias, catalog)
	}
	if _, err := db.ExecContext(t.Context(), attach); err != nil {
		t.Fatalf("attach: %v", err)
	}
	return db
}

func TestClientReadsOverWebSocket(t *testing.T) {
	db := newClient(t, newTestServer(t))

	var count int
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM app.main.users`).Scan(&count); err != nil {
		t.Fatalf("query over ws: %v", err)
	}
	if count != 2 {
		t.Errorf("app.main.users: got %d rows, want 2", count)
	}
}

func TestClientWritesOverWebSocket(t *testing.T) {
	db := newClient(t, newTestServer(t))

	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO app.main.users VALUES (3, 'Katherine')`); err != nil {
		t.Fatalf("insert over ws: %v", err)
	}

	var name string
	if err := db.QueryRowContext(t.Context(),
		`SELECT name FROM app.main.users WHERE id = 3`).Scan(&name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != "Katherine" {
		t.Errorf("got %q, want %q", name, "Katherine")
	}
}

// The permission is checked over the whole chain: client -> ws -> reactor ->
// bridge, rather than against a Session in-process.
func TestReadOnlyTableRejectsWriteOverWebSocket(t *testing.T) {
	db := newClient(t, newTestServer(t))

	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO app.main.audit_log VALUES (2, 'logout')`); err == nil {
		t.Fatal("insert into read-only table succeeded over ws")
	} else {
		t.Logf("rejected as expected: %v", err)
	}
}
