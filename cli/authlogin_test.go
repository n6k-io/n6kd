package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/n6k-io/n6kd/auth"
	"github.com/n6k-io/n6kd/auth/authtest"
	"github.com/n6k-io/n6kd/engine"
	"github.com/n6k-io/n6kd/perms"
	"github.com/n6k-io/n6kd/server"
)

func TestProtectedResourceURL(t *testing.T) {
	for _, test := range []struct {
		target, scope, url string
	}{
		{"localhost:8080", "localhost:8080", "http://localhost:8080"},
		{"127.0.0.1:8080", "127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"data.acme.com", "data.acme.com", "https://data.acme.com"},
		{"ws://data.acme.com/ws", "data.acme.com", "http://data.acme.com"},
		{"wss://data.acme.com:9/ws", "data.acme.com:9", "https://data.acme.com:9"},
		{"n6k://h:1", "h:1", "http://h:1"},
		{"n6ks://h", "h", "https://h"},
	} {
		scope, url, err := protectedResourceURL(test.target)
		if err != nil {
			t.Errorf("%s: %v", test.target, err)
			continue
		}
		want := test.url + server.ProtectedResourcePath
		if scope != test.scope || url != want {
			t.Errorf("%s: got %s, %s, want %s, %s", test.target, scope, url, test.scope, want)
		}
	}
	for _, target := range []string{"ftp://h", "http://"} {
		if _, _, err := protectedResourceURL(target); err == nil {
			t.Errorf("%s: want an error", target)
		}
	}
}

func TestLoginScopes(t *testing.T) {
	if got := loginScopes(nil); !slices.Equal(got, []string{"openid", "email", "offline_access"}) {
		t.Errorf("no list: got %v", got)
	}
	if got := loginScopes([]string{"openid", "email", "profile"}); !slices.Equal(got, []string{"openid", "email"}) {
		t.Errorf("list without offline_access: got %v", got)
	}
}

func TestAuthLoginServerRefusesServerWithoutSignIn(t *testing.T) {
	if os.Getenv(engine.ExtensionDirEnv) == "" {
		t.Skipf("%s not set", engine.ExtensionDirEnv)
	}
	t.Setenv("HOME", t.TempDir())
	url := startServer(t, engine.Options{Boot: []engine.Step{{SQL: "ATTACH ':memory:' AS app"}}})

	cmd := newAuthCmd()
	cmd.SetArgs([]string{"login", url})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.ExecuteContext(t.Context())
	if err == nil || !strings.Contains(err.Error(), "--oidc-client-id") {
		t.Errorf("got %v, want it to name --oidc-client-id", err)
	}
}

func TestAuthLoginServerThenAttach(t *testing.T) {
	if os.Getenv(engine.ExtensionDirEnv) == "" {
		t.Skipf("%s not set", engine.ExtensionDirEnv)
	}
	t.Setenv("HOME", t.TempDir())

	issuer := authtest.NewIssuer(t)
	issuer.LoginAudience = "n6kd"
	issuer.LoginClaims = map[string]any{"sub": "alice"}
	verifier, err := auth.NewOIDC(t.Context(), issuer.URL, "n6kd")
	if err != nil {
		t.Fatal(err)
	}
	schemaPath := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(`
ATTACH ':memory:' AS app;
CREATE TABLE app.main.orders (id INTEGER PRIMARY KEY, owner VARCHAR);
INSERT INTO app.main.orders VALUES (1, 'alice'), (2, 'bob');
`), 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err := perms.Parse([]byte(`
catalogs:
  app:
    tables:
      main.orders:
        select: "owner = $sub"
`))
	if err != nil {
		t.Fatal(err)
	}
	url := startServerWith(t, engine.Options{Boot: []engine.Step{{Path: schemaPath}}, Perms: rules, Verifier: verifier},
		server.Config{ProtectedResource: &server.ProtectedResource{
			Resource:             "n6kd",
			AuthorizationServers: []string{issuer.URL},
			ClientID:             "n6k-cli",
		}})

	var out bytes.Buffer
	cmd := newAuthCmd()
	cmd.SetArgs([]string{"login", url})
	cmd.SetErr(&out)
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("auth login: %v", err)
	}
	if want := "Sign in at " + issuer.URL + "/activate (code: TEST-CODE)"; !strings.Contains(out.String(), want) {
		t.Errorf("output %q, want it to contain %q", out.String(), want)
	}

	db, err := engine.OpenClient(t.Context(), zaptest.NewLogger(t).Sugar())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	authority := strings.TrimSuffix(strings.TrimPrefix(url, "ws://"), "/ws")
	if _, err := db.ExecContext(t.Context(), "ATTACH 'n6k://"+authority+"' AS remote (TYPE n6k, CATALOG 'app')"); err != nil {
		t.Fatalf("attach with no token: %v", err)
	}
	var owner string
	if err := db.QueryRowContext(t.Context(), "SELECT owner FROM remote.main.orders").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "alice" {
		t.Errorf("owner %q, want alice", owner)
	}

	cs, _ := startMCP(t, map[string]string{"orders": url})
	mustCallTool(t, cs, "attach", map[string]any{"server": "orders", "catalog": "app", "alias": "remote"})
	got := mustCallTool(t, cs, "query", map[string]any{"sql": "SELECT owner FROM remote.main.orders"})
	if want := "owner\nalice\n(1 row)"; got != want {
		t.Errorf("mcp query returned %q, want %q", got, want)
	}
}
