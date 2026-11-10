package server

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/n6k-io/n6kd/engine"
)

const spikeUpstreamSQL = `
CREATE TABLE users (id INTEGER PRIMARY KEY, name VARCHAR);
INSERT INTO users VALUES (1, 'Ada'), (2, 'Grace');
CREATE TABLE big AS SELECT range AS i FROM range(300000);
CREATE MACRO double_it(x) AS TABLE SELECT x * 2 AS v;
`

func spikeDirectServer(t *testing.T, schema string) *httptest.Server {
	t.Helper()
	if os.Getenv(engine.ExtensionDirEnv) == "" {
		t.Skipf("%s not set", engine.ExtensionDirEnv)
	}
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte(schema), 0o600); err != nil {
		t.Fatal(err)
	}
	log := zaptest.NewLogger(t).Sugar()
	eng, err := engine.Open(t.Context(), engine.Options{Boot: []engine.Step{{Path: path}}}, log)
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Logf("serves catalogs %v", eng.Catalogs())
	srv := httptest.NewServer(newRouter(Config{}, eng, log))
	t.Cleanup(func() {
		srv.Close()
		if err := eng.Close(); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})
	return srv
}

func TestSpikeGatewayThroughNestedAttach(t *testing.T) {
	up := spikeDirectServer(t, spikeUpstreamSQL)
	gw := spikeDirectServer(t, fmt.Sprintf(`
ATTACH 'n6k://%s' AS remote (TYPE n6k, CATALOG 'memory');
CREATE TABLE notes (id INTEGER, body VARCHAR);
INSERT INTO notes VALUES (1, 'agent scratch');
`, strings.TrimPrefix(up.URL, "http://")))

	db := newClientAs(t, gw, "r", "remote")
	authority := strings.TrimPrefix(gw.URL, "http://")
	if _, err := db.ExecContext(t.Context(),
		fmt.Sprintf("ATTACH 'n6k://%s' AS m (TYPE n6k, CATALOG 'memory')", authority)); err != nil {
		t.Fatalf("attach gateway memory: %v", err)
	}

	if _, err := db.ExecContext(t.Context(),
		fmt.Sprintf("ATTACH 'n6k://%s' AS u (TYPE n6k, CATALOG 'memory')", strings.TrimPrefix(up.URL, "http://"))); err != nil {
		t.Fatalf("attach upstream: %v", err)
	}

	scalar := func(t *testing.T, q string) string {
		t.Helper()
		var v string
		if err := db.QueryRowContext(t.Context(), q).Scan(&v); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return v
	}
	cases := []struct{ name, query, want string }{
		{"scan", `SELECT count(*)::VARCHAR FROM r.main.users`, "2"},
		{"stream large scan", `SELECT sum(i)::VARCHAR FROM r.main.big`, "44999850000"},
		{"query op", `SELECT count(*)::VARCHAR FROM n6k_catalog_query('r', 'SELECT * FROM main.users')`, "2"},
		{"query op qualified", `SELECT count(*)::VARCHAR FROM n6k_catalog_query('r', 'SELECT * FROM remote.main.users')`, "2"},
		{"query op upstream direct", `SELECT count(*)::VARCHAR FROM n6k_catalog_query('u', 'SELECT * FROM main.users')`, "2"},
		{"rpc macro", `SELECT v::VARCHAR FROM n6k_catalog_rpc('r', 'double_it', 21)`, "42"},
		{"gateway memory", `SELECT body FROM m.main.notes`, "agent scratch"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := scalar(t, c.query); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	t.Run("write", func(t *testing.T) {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO r.main.users VALUES (3, 'Linus')`); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if got := scalar(t, `SELECT count(*)::VARCHAR FROM r.main.users`); got != "3" {
			t.Errorf("got %q, want 3", got)
		}
	})
}
