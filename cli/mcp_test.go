package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap/zaptest"

	"github.com/n6k-io/n6kd/auth"
	"github.com/n6k-io/n6kd/auth/authtest"
	"github.com/n6k-io/n6kd/engine"
	"github.com/n6k-io/n6kd/perms"
	"github.com/n6k-io/n6kd/server"
)

func startServer(t *testing.T, opts engine.Options) string {
	t.Helper()
	log := zaptest.NewLogger(t).Sugar()
	eng, err := engine.Open(t.Context(), opts, log)
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, server.Config{Addr: addr}, eng, log) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("server: %v", err)
		}
		if err := eng.Close(); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			return "ws://" + addr + "/ws"
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func storeSecret(t *testing.T, scope, token string) {
	t.Helper()
	db, err := engine.OpenClient(t.Context(), zaptest.NewLogger(t).Sugar())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	stmt := "CREATE PERSISTENT SECRET mcp_test (TYPE n6k, TOKEN " + sqlString(token) + ", SCOPE " + sqlString(scope) + ")"
	if _, err := db.ExecContext(t.Context(), stmt); err != nil {
		t.Fatal(err)
	}
}

func startMCP(t *testing.T, server string) *mcp.ClientSession {
	t.Helper()
	return startMCPWith(t, mcpFlags{server: server})
}

func startMCPWith(t *testing.T, flags mcpFlags) *mcp.ClientSession {
	t.Helper()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- runMCP(ctx, flags, serverTransport, zaptest.NewLogger(t).Sugar())
	}()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		cancel()
		<-done
	})
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, error) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.StructuredContent != nil {
		t.Errorf("%s returned structured content %v, want text only", name, res.StructuredContent)
	}
	var texts []string
	for _, c := range res.Content {
		text, ok := c.(*mcp.TextContent)
		if !ok {
			t.Fatalf("%s returned %T, want text", name, c)
		}
		texts = append(texts, text.Text)
	}
	if res.IsError {
		return "", errors.New(strings.Join(texts, "\n"))
	}
	return strings.Join(texts, "\n"), nil
}

func mustCallTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	text, err := callTool(t, cs, name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return text
}

func startOrdersServer(t *testing.T) (string, *authtest.Issuer) {
	t.Helper()
	if os.Getenv(engine.ExtensionDirEnv) == "" {
		t.Skipf("%s not set", engine.ExtensionDirEnv)
	}
	t.Setenv("HOME", t.TempDir())

	issuer := authtest.NewIssuer(t)
	verifier, err := auth.NewOIDC(t.Context(), issuer.URL, "n6kd")
	if err != nil {
		t.Fatal(err)
	}
	schemaPath := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(`
ATTACH ':memory:' AS app;
CREATE TABLE app.main.orders (id INTEGER PRIMARY KEY, owner VARCHAR);
INSERT INTO app.main.orders VALUES (1, 'alice'), (2, 'bob'), (3, 'alice');
`), 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err := perms.Parse([]byte(`
tables:
  app.orders:
    select: "owner = $sub"
`))
	if err != nil {
		t.Fatal(err)
	}
	return startServer(t, engine.Options{Boot: []engine.Step{{Path: schemaPath}}, Perms: rules, Verifier: verifier}), issuer
}

func TestMCPConnectsWithScopedSecret(t *testing.T) {
	server, issuer := startOrdersServer(t)
	scope := strings.TrimSuffix(strings.TrimPrefix(server, "ws://"), "/ws")
	storeSecret(t, scope, issuer.Token(t, "n6kd", map[string]any{"sub": "alice"}))

	cs := startMCP(t, server)

	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if tool.OutputSchema != nil {
			t.Errorf("%s has an output schema %v, want none", tool.Name, tool.OutputSchema)
		}
	}
	if got := strings.Join(names, ","); got != "attach,execute,list_catalogs,query" {
		t.Errorf("tools %s", got)
	}

	if got := mustCallTool(t, cs, "list_catalogs", nil); got != "app" {
		t.Fatalf("list_catalogs %q, want %q", got, "app")
	}

	attached := mustCallTool(t, cs, "attach", map[string]any{"catalog": "app", "alias": "remote"})
	for _, want := range []string{
		"ran `ATTACH '' AS \"remote\" (TYPE n6k, wsFd ",
		"CATALOG 'app')` successfully",
		"\n\nremote.main.orders: id INTEGER, owner VARCHAR",
	} {
		if !strings.Contains(attached, want) {
			t.Errorf("attach returned %q, want it to contain %q", attached, want)
		}
	}

	got := mustCallTool(t, cs, "query", map[string]any{"sql": "SELECT id, owner, NULL AS note FROM remote.main.orders ORDER BY id"})
	if want := "id,owner,note\n1,alice,NULL\n3,alice,NULL\n(2 rows)"; got != want {
		t.Fatalf("query returned %q, want %q", got, want)
	}

	if got := mustCallTool(t, cs, "execute", map[string]any{"sql": "CREATE TABLE local_notes AS SELECT * FROM remote.main.orders"}); !strings.HasPrefix(got, "ran `CREATE TABLE local_notes") {
		t.Fatalf("execute returned %q", got)
	}
	if got := mustCallTool(t, cs, "query", map[string]any{"sql": "SELECT count(*) AS n FROM local_notes"}); got != "n\n2\n(1 row)" {
		t.Fatalf("local table query returned %q", got)
	}
}

func TestMCPConnectsWithAPIKey(t *testing.T) {
	if os.Getenv(engine.ExtensionDirEnv) == "" {
		t.Skipf("%s not set", engine.ExtensionDirEnv)
	}
	t.Setenv("HOME", t.TempDir())
	schemaPath := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(`
ATTACH ':memory:' AS app;
CREATE TABLE app.main.users (id INTEGER PRIMARY KEY, name VARCHAR);
INSERT INTO app.main.users VALUES (1, 'Ada'), (2, 'Grace');
`), 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err := perms.Parse([]byte("tables:\n  app.users:\n    select: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	server := startServer(t, engine.Options{Boot: []engine.Step{{Path: schemaPath}}, Perms: rules, APIKey: "s3cret"})

	for _, test := range []struct {
		apiKey  string
		wantErr bool
	}{
		{"s3cret", false},
		{"wrong", true},
	} {
		t.Run(test.apiKey, func(t *testing.T) {
			cs := startMCPWith(t, mcpFlags{server: server, apiKey: test.apiKey})
			got, err := callTool(t, cs, "list_catalogs", nil)
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "unauthorized") {
					t.Fatalf("got %v, want an unauthorized error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != "app" {
				t.Fatalf("list_catalogs %q, want %q", got, "app")
			}
		})
	}
}

func TestMCPWithoutSecretIsUnauthorized(t *testing.T) {
	server, _ := startOrdersServer(t)
	cs := startMCP(t, server)

	_, err := callTool(t, cs, "list_catalogs", nil)
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("got %v, want an unauthorized error", err)
	}
}
