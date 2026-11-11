package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	return startServerWith(t, opts, server.Config{})
}

func startServerWith(t *testing.T, opts engine.Options, cfg server.Config) string {
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
	cfg.Addr = addr
	go func() { done <- server.Run(ctx, cfg, eng, log) }()
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

func loginAPIKey(t *testing.T, server, key string) {
	t.Helper()
	cmd := newAuthCmd()
	cmd.SetArgs([]string{"login", "--server", server, "--api-key", key})
	cmd.SetErr(io.Discard)
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("auth login --server: %v", err)
	}
}

func writeMCPConfig(t *testing.T, servers map[string]string) string {
	t.Helper()
	config := mcpConfig{Servers: map[string]mcpServerEntry{}}
	for name, url := range servers {
		config.Servers[name] = mcpServerEntry{URL: url}
	}
	path := filepath.Join(t.TempDir(), "mcp.yml")
	if err := saveMCPConfig(path, config); err != nil {
		t.Fatal(err)
	}
	return path
}

func startMCP(t *testing.T, servers map[string]string) (*mcp.ClientSession, string) {
	t.Helper()
	configPath := writeMCPConfig(t, servers)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- runMCP(ctx, configPath, "127.0.0.1:0", serverTransport, zaptest.NewLogger(t).Sugar())
	}()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	return cs, configPath
}

func toolNames(t *testing.T, cs *mcp.ClientSession) string {
	t.Helper()
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return strings.Join(names, ",")
}

const localTools = "add_server,attach,execute,list_catalogs,list_servers,query,remove_server,serve_info"

func requireExtensions(t *testing.T) {
	t.Helper()
	if os.Getenv(engine.ExtensionDirEnv) == "" {
		t.Skipf("%s not set", engine.ExtensionDirEnv)
	}
	t.Setenv("HOME", t.TempDir())
}

func startEchoServer(t *testing.T, opts engine.Options) string {
	t.Helper()
	type echoInput struct {
		Text string `json:"text"`
	}
	remote := mcp.NewServer(&mcp.Implementation{Name: "remote"}, nil)
	mcp.AddTool(remote, &mcp.Tool{Name: "echo", Description: "echoes text"},
		func(_ context.Context, _ *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, any, error) {
			return textResult("echo: " + in.Text), nil, nil
		})
	return startServerWith(t, opts, server.Config{MCP: remote})
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
catalogs:
  app:
    tables:
      main.orders:
        select: "owner = $sub"
`))
	if err != nil {
		t.Fatal(err)
	}
	return startServer(t, engine.Options{Boot: []engine.Step{{Path: schemaPath}}, Perms: rules, Verifier: verifier}), issuer
}

func TestMCPConnectsWithScopedSecret(t *testing.T) {
	orders, issuer := startOrdersServer(t)
	loginAPIKey(t, orders, issuer.Token(t, "n6kd", map[string]any{"sub": "alice"}))

	cs, _ := startMCP(t, map[string]string{"orders": orders})

	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.OutputSchema != nil {
			t.Errorf("%s has an output schema %v, want none", tool.Name, tool.OutputSchema)
		}
	}
	if got := toolNames(t, cs); got != localTools {
		t.Errorf("tools %s, want %s", got, localTools)
	}

	if got := mustCallTool(t, cs, "list_catalogs", map[string]any{"server": "orders"}); got != "app" {
		t.Fatalf("list_catalogs %q, want %q", got, "app")
	}

	attached := mustCallTool(t, cs, "attach", map[string]any{"server": "orders", "catalog": "app", "alias": "remote"})
	for _, want := range []string{
		"ran `ATTACH '' AS \"remote\" (TYPE n6k, CONNECTION '",
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

	if got := mustCallTool(t, cs, "list_servers", nil); got != "orders: "+orders+", connected, 0 tools, attached: remote" {
		t.Errorf("list_servers %q", got)
	}
}

func TestMCPConnectsWithAPIKey(t *testing.T) {
	requireExtensions(t)
	schemaPath := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(`
ATTACH ':memory:' AS app;
CREATE TABLE app.main.users (id INTEGER PRIMARY KEY, name VARCHAR);
INSERT INTO app.main.users VALUES (1, 'Ada'), (2, 'Grace');
`), 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err := perms.Parse([]byte("catalogs:\n  app:\n    tables:\n      main.users:\n        select: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	users := startServer(t, engine.Options{Boot: []engine.Step{{Path: schemaPath}}, Perms: rules, APIKey: "s3cret"})

	for _, test := range []struct {
		apiKey  string
		wantErr bool
	}{
		{"s3cret", false},
		{"wrong", true},
	} {
		t.Run(test.apiKey, func(t *testing.T) {
			loginAPIKey(t, users, test.apiKey)
			cs, _ := startMCP(t, map[string]string{"users": users})
			got, err := callTool(t, cs, "list_catalogs", map[string]any{"server": "users"})
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
	orders, _ := startOrdersServer(t)
	cs, _ := startMCP(t, map[string]string{"orders": orders})

	_, err := callTool(t, cs, "list_catalogs", map[string]any{"server": "orders"})
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("got %v, want an unauthorized error", err)
	}
}

func TestMCPForwardsRemoteTools(t *testing.T) {
	requireExtensions(t)
	shop := startEchoServer(t, engine.Options{APIKey: "s3cret"})

	for _, test := range []struct {
		apiKey string
		tools  string
	}{
		{"s3cret", localTools + ",shop_echo"},
		{"wrong", localTools},
	} {
		t.Run(test.apiKey, func(t *testing.T) {
			loginAPIKey(t, shop, test.apiKey)
			cs, _ := startMCP(t, map[string]string{"shop": shop})
			if got := toolNames(t, cs); got != test.tools {
				t.Fatalf("tools %s, want %s", got, test.tools)
			}
			if test.apiKey != "s3cret" {
				return
			}
			if got := mustCallTool(t, cs, "shop_echo", map[string]any{"text": "hi"}); got != "echo: hi" {
				t.Fatalf("echo returned %q", got)
			}
		})
	}
}

func TestMCPAddAndRemoveServer(t *testing.T) {
	requireExtensions(t)
	shop := startEchoServer(t, engine.Options{})
	cs, configPath := startMCP(t, nil)

	if got := mustCallTool(t, cs, "list_servers", nil); got != "no servers; add one with add_server" {
		t.Fatalf("list_servers %q", got)
	}

	added := mustCallTool(t, cs, "add_server", map[string]any{"name": "shop", "url": shop, "persist": true})
	if want := "connected shop to " + shop + ", serving 1 tool, saved to " + configPath; added != want {
		t.Fatalf("add_server %q, want %q", added, want)
	}
	if got := toolNames(t, cs); got != localTools+",shop_echo" {
		t.Fatalf("tools after add_server %s", got)
	}
	if got := mustCallTool(t, cs, "shop_echo", map[string]any{"text": "hi"}); got != "echo: hi" {
		t.Fatalf("echo returned %q", got)
	}
	saved, err := loadMCPConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.Servers["shop"].URL; got != shop {
		t.Fatalf("saved url %q, want %q", got, shop)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("config mode %o, want 600", mode)
	}

	if _, err := callTool(t, cs, "add_server", map[string]any{"name": "shop", "url": shop}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second add_server got %v, want already exists", err)
	}
	if _, err := callTool(t, cs, "add_server", map[string]any{"name": "bad name", "url": shop}); err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("add_server with a bad name got %v", err)
	}

	if got := mustCallTool(t, cs, "remove_server", map[string]any{"name": "shop", "persist": true}); got != "removed shop from "+configPath {
		t.Fatalf("remove_server %q", got)
	}
	if got := toolNames(t, cs); got != localTools {
		t.Fatalf("tools after remove_server %s", got)
	}
	saved, err = loadMCPConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Servers) != 0 {
		t.Fatalf("config still has %v", saved.Servers)
	}
	if _, err := callTool(t, cs, "remove_server", map[string]any{"name": "shop"}); err == nil || !strings.Contains(err.Error(), "no server named") {
		t.Fatalf("second remove_server got %v", err)
	}
}

func TestMCPTwoServers(t *testing.T) {
	orders, issuer := startOrdersServer(t)
	loginAPIKey(t, orders, issuer.Token(t, "n6kd", map[string]any{"sub": "alice"}))
	shop := startEchoServer(t, engine.Options{})
	cs, _ := startMCP(t, map[string]string{"orders": orders, "shop": shop})

	if got := toolNames(t, cs); got != localTools+",shop_echo" {
		t.Fatalf("tools %s", got)
	}
	if got := mustCallTool(t, cs, "list_catalogs", nil); got != "orders.app\nshop.memory" {
		t.Fatalf("list_catalogs %q", got)
	}
	mustCallTool(t, cs, "attach", map[string]any{"server": "orders", "catalog": "app"})
	if _, err := callTool(t, cs, "attach", map[string]any{"server": "shop", "catalog": "memory", "alias": "app"}); err == nil || !strings.Contains(err.Error(), "already attached from orders") {
		t.Fatalf("attach with a taken alias got %v", err)
	}
	if _, err := callTool(t, cs, "attach", map[string]any{"server": "nope", "catalog": "app"}); err == nil || !strings.Contains(err.Error(), "no server named") {
		t.Fatalf("attach from an unknown server got %v", err)
	}
}

type dropProxy struct {
	listener net.Listener
	mu       sync.Mutex
	live     []net.Conn
}

func startDropProxy(t *testing.T, target string) *dropProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &dropProxy{listener: listener}
	t.Cleanup(func() {
		_ = listener.Close()
		p.drop()
	})
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			upstream, err := net.Dial("tcp", target)
			if err != nil {
				_ = client.Close()
				continue
			}
			p.mu.Lock()
			p.live = append(p.live, client, upstream)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close() }()
			go func() { _, _ = io.Copy(client, upstream); _ = client.Close() }()
		}
	}()
	return p
}

func (p *dropProxy) drop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, conn := range p.live {
		_ = conn.Close()
	}
	p.live = nil
}

func startUsersEchoServer(t *testing.T) string {
	t.Helper()
	requireExtensions(t)
	schemaPath := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(`
ATTACH ':memory:' AS app;
CREATE TABLE app.main.users (id INTEGER PRIMARY KEY, name VARCHAR);
INSERT INTO app.main.users VALUES (1, 'Ada'), (2, 'Grace');
`), 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err := perms.Parse([]byte("catalogs:\n  app:\n    tables:\n      main.users:\n        select: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	return startEchoServer(t, engine.Options{Boot: []engine.Step{{Path: schemaPath}}, Perms: rules, APIKey: "s3cret"})
}

func TestMCPRecoversFromDroppedConnection(t *testing.T) {
	direct := startUsersEchoServer(t)
	proxy := startDropProxy(t, strings.TrimSuffix(strings.TrimPrefix(direct, "ws://"), "/ws"))
	url := "ws://" + proxy.listener.Addr().String() + "/ws"
	loginAPIKey(t, url, "s3cret")
	cs, _ := startMCP(t, map[string]string{"users": url})

	const count, wantCount = "SELECT count(*) AS n FROM remote.main.users", "n\n2\n(1 row)"
	echoArgs := map[string]any{"text": "hi"}
	mustCallTool(t, cs, "attach", map[string]any{"server": "users", "catalog": "app", "alias": "remote"})
	if got := mustCallTool(t, cs, "query", map[string]any{"sql": count}); got != wantCount {
		t.Fatalf("query before the drop %q", got)
	}
	if got := mustCallTool(t, cs, "users_echo", echoArgs); got != "echo: hi" {
		t.Fatalf("echo before the drop %q", got)
	}

	proxy.drop()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(mustCallTool(t, cs, "list_servers", nil), ", disconnected,") {
		if time.Now().After(deadline) {
			t.Fatal("list_servers never showed the drop")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := mustCallTool(t, cs, "users_echo", echoArgs); got != "echo: hi" {
		t.Errorf("echo after the drop %q", got)
	}
	if got := mustCallTool(t, cs, "query", map[string]any{"sql": count}); got != wantCount {
		t.Errorf("query after the drop %q", got)
	}
	if got := mustCallTool(t, cs, "list_servers", nil); got != "users: "+url+", connected, 1 tool, attached: remote" {
		t.Errorf("list_servers after the drop %q", got)
	}
}

func TestMCPRemoveServerClosesConnection(t *testing.T) {
	url := startUsersEchoServer(t)
	loginAPIKey(t, url, "s3cret")
	cs, _ := startMCP(t, map[string]string{"users": url})

	const connections = "SELECT count(*) AS n FROM n6k_connections()"
	mustCallTool(t, cs, "attach", map[string]any{"server": "users", "catalog": "app", "alias": "remote"})
	if got := mustCallTool(t, cs, "query", map[string]any{"sql": connections}); got != "n\n1\n(1 row)" {
		t.Fatalf("connections before remove_server %q", got)
	}
	mustCallTool(t, cs, "remove_server", map[string]any{"name": "users"})
	if got := mustCallTool(t, cs, "query", map[string]any{"sql": connections}); got != "n\n0\n(1 row)" {
		t.Errorf("connections after remove_server %q", got)
	}
}

func TestMCPSameURLUnderTwoNames(t *testing.T) {
	url := startUsersEchoServer(t)
	loginAPIKey(t, url, "s3cret")
	cs, _ := startMCP(t, map[string]string{"first": url})

	_, err := callTool(t, cs, "add_server", map[string]any{"name": "second", "url": url})
	if want := `is already added as server "first"`; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("add_server got %v, want %q", err, want)
	}
	if got := mustCallTool(t, cs, "first_echo", map[string]any{"text": "hi"}); got != "echo: hi" {
		t.Errorf("echo on the first server %q", got)
	}
}

func TestExtensionURL(t *testing.T) {
	for raw, want := range map[string]string{
		"ws://host:1/ws":            "n6k://host:1",
		"wss://host/prefix/ws":      "n6ks://host/prefix",
		"ws://host:1/ws?catalogs=a": "n6k://host:1?catalogs=a",
		"ws://host:1/other":         "",
		"http://host:1/ws":          "",
	} {
		got, err := extensionURL(raw)
		if want == "" {
			if err == nil {
				t.Errorf("%s: got %q, want an error", raw, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestMCPConfigIsStrict(t *testing.T) {
	for _, test := range []struct {
		yaml, want string
	}{
		{"servers:\n  shop:\n    url: ws://h:1/ws\n    token: x\n", "unknown field"},
		{"servers:\n  bad name:\n    url: ws://h:1/ws\n", "must match"},
		{"servers:\n  shop:\n    url: /ws\n", "has no host"},
	} {
		path := filepath.Join(t.TempDir(), "mcp.yml")
		if err := os.WriteFile(path, []byte(test.yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadMCPConfig(path); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%q: got %v, want %q", test.yaml, err, test.want)
		}
	}
	config, err := loadMCPConfig(filepath.Join(t.TempDir(), "missing.yml"))
	if err != nil || len(config.Servers) != 0 {
		t.Errorf("missing config: %v, %v", config, err)
	}
}

func TestAuthLoginFlags(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"login"}, "SERVER, --issuer, or --server with --api-key, is required"},
		{[]string{"login", "h:1", "--issuer", "https://i"}, "SERVER cannot be used with --issuer, --server or --api-key"},
		{[]string{"login", "--server", "ws://h:1/ws"}, "--server and --api-key must be given together"},
		{[]string{"login", "--issuer", "https://i", "--api-key", "k"}, "--issuer cannot be used with --server or --api-key"},
	} {
		cmd := newAuthCmd()
		cmd.SetArgs(test.args)
		cmd.SetErr(io.Discard)
		cmd.SetOut(io.Discard)
		if err := cmd.ExecuteContext(t.Context()); err == nil || err.Error() != test.want {
			t.Errorf("%v: got %v, want %q", test.args, err, test.want)
		}
	}
}

func TestMCPServesPermsDocs(t *testing.T) {
	requireExtensions(t)
	schemaPath := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(`
ATTACH ':memory:' AS app;
CREATE TABLE app.main.orders (id INTEGER PRIMARY KEY, status VARCHAR);
CREATE TABLE app.main.users (id INTEGER PRIMARY KEY);
`), 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err := perms.Parse([]byte(`
catalogs:
  app:
    description: "Orders and their customers"
    hints: "Amounts are in cents."
    prompts:
      open_orders:
        description: "List open orders"
        args:
          customer: { description: "One customer" }
        template: "List open orders{{if .customer}} for {{.customer}}{{end}}."
    tables:
      main.orders:
        select: true
        description: "One row per order"
        columns: { status: "open | shipped" }
        examples: ["SELECT * FROM app.main.orders"]
      main.users:
        select: true
`))
	if err != nil {
		t.Fatal(err)
	}
	opts := engine.Options{Boot: []engine.Step{{Path: schemaPath}}, Perms: rules, APIKey: "s3cret"}
	shop := startServerWith(t, opts, server.Config{MCP: server.NewMCP(rules, "test")})
	loginAPIKey(t, shop, "s3cret")
	cs, _ := startMCP(t, map[string]string{"shop": shop})

	prompts, err := cs.ListPrompts(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts.Prompts) != 1 || prompts.Prompts[0].Name != "shop_app_open_orders" {
		t.Fatalf("prompts %+v", prompts.Prompts)
	}
	for args, want := range map[string]string{"": "List open orders.", "bob": "List open orders for bob."} {
		params := &mcp.GetPromptParams{Name: "shop_app_open_orders"}
		if args != "" {
			params.Arguments = map[string]string{"customer": args}
		}
		got, err := cs.GetPrompt(t.Context(), params)
		if err != nil {
			t.Fatal(err)
		}
		if text := got.Messages[0].Content.(*mcp.TextContent).Text; text != want {
			t.Errorf("prompt with %q: %q, want %q", args, text, want)
		}
	}

	resources, err := cs.ListResources(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var uris []string
	for _, r := range resources.Resources {
		uris = append(uris, r.URI)
	}
	slices.Sort(uris)
	if got, want := strings.Join(uris, ","), "n6k://shop/app,n6k://shop/app/main.orders,n6k://shop/app?format=json"; got != want {
		t.Fatalf("resources %s, want %s", got, want)
	}
	read, err := cs.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "n6k://shop/app"})
	if err != nil {
		t.Fatal(err)
	}
	if text := read.Contents[0].Text; !strings.Contains(text, "## Hints\n\nAmounts are in cents.") || read.Contents[0].URI != "n6k://shop/app" {
		t.Errorf("read %+v", read.Contents[0])
	}

	attached := mustCallTool(t, cs, "attach", map[string]any{"server": "shop", "catalog": "app", "alias": "remote"})
	want := "successfully\n\nOrders and their customers\n\n" +
		"remote.main.orders: id INTEGER, status VARCHAR\n  One row per order\n  status: open | shipped\n" +
		"remote.main.users: id INTEGER\n\n" +
		"full hints and examples: n6k://shop/app"
	if !strings.HasSuffix(attached, want) {
		t.Errorf("attach returned %q, want suffix %q", attached, want)
	}
}

func TestNewMCPWithoutDocs(t *testing.T) {
	rules, err := perms.Parse([]byte("catalogs:\n  app:\n    tables:\n      main.users:\n        select: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if server.NewMCP(rules, "test") != nil || server.NewMCP(nil, "test") != nil {
		t.Fatal("NewMCP served a server with no docs")
	}
}

func serveClient(t *testing.T, info serveInfo, token string) (*sql.DB, string, error) {
	t.Helper()
	db, err := engine.OpenClient(t.Context(), zaptest.NewLogger(t).Sugar())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	target, err := extensionURL(info.URL)
	if err != nil {
		t.Fatal(err)
	}
	authority := strings.TrimPrefix(target, "n6k://")
	if _, err := db.ExecContext(t.Context(), fmt.Sprintf("CREATE SECRET (TYPE n6k, TOKEN '%s', SCOPE '%s')", token, authority)); err != nil {
		t.Fatal(err)
	}
	var id string
	err = db.QueryRowContext(t.Context(), "SELECT connection_id::VARCHAR FROM n6k_connect(url => ?)", target).Scan(&id)
	return db, id, err
}

func TestMCPServesItsDuckDB(t *testing.T) {
	requireExtensions(t)
	schemaPath := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(schemaPath, []byte(`
ATTACH ':memory:' AS app;
CREATE TABLE app.main.users (id INTEGER PRIMARY KEY, name VARCHAR);
INSERT INTO app.main.users VALUES (1, 'Ada'), (2, 'Grace');
`), 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err := perms.Parse([]byte("catalogs:\n  app:\n    tables:\n      main.users:\n        select: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	users := startServer(t, engine.Options{Boot: []engine.Step{{Path: schemaPath}}, Perms: rules, APIKey: "s3cret"})
	loginAPIKey(t, users, "s3cret")
	cs, _ := startMCP(t, map[string]string{"users": users})
	mustCallTool(t, cs, "attach", map[string]any{"server": "users", "catalog": "app", "alias": "first"})

	var info serveInfo
	if err := json.Unmarshal([]byte(mustCallTool(t, cs, "serve_info", nil)), &info); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(info.URL, "ws://127.0.0.1:") || !slices.Contains(info.Catalogs, "first") {
		t.Fatalf("serve_info %+v", info)
	}

	t.Run("wrong token", func(t *testing.T) {
		if _, _, err := serveClient(t, info, "wrong"); err == nil || !strings.Contains(err.Error(), "unauthorized") {
			t.Fatalf("got %v, want an unauthorized error", err)
		}
	})

	db, id, err := serveClient(t, info, info.Token)
	if err != nil {
		t.Fatal(err)
	}
	count := func(alias string) int {
		t.Helper()
		if _, err := db.ExecContext(t.Context(), fmt.Sprintf("ATTACH '' AS c_%s (TYPE n6k, CONNECTION '%s', CATALOG '%s')", alias, id, alias)); err != nil {
			t.Fatalf("attach %s: %v", alias, err)
		}
		var n int
		if err := db.QueryRowContext(t.Context(), fmt.Sprintf("SELECT count(*) FROM c_%s.main.users", alias)).Scan(&n); err != nil {
			t.Fatalf("query %s: %v", alias, err)
		}
		return n
	}
	if n := count("first"); n != 2 {
		t.Fatalf("first has %d users, want 2", n)
	}

	mustCallTool(t, cs, "attach", map[string]any{"server": "users", "catalog": "app", "alias": "second"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		var listed bool
		if err := db.QueryRowContext(t.Context(),
			"SELECT count(*) > 0 FROM n6k_list_catalogs(connection => ?::UUID) WHERE catalog_name = 'second'", id).Scan(&listed); err != nil {
			t.Fatal(err)
		}
		if listed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second never listed for the connected client")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := count("second"); n != 2 {
		t.Fatalf("second has %d users, want 2", n)
	}

	mustCallTool(t, cs, "remove_server", map[string]any{"name": "users"})
	if err := json.Unmarshal([]byte(mustCallTool(t, cs, "serve_info", nil)), &info); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(info.Catalogs, []string{"memory"}) {
		t.Fatalf("catalogs after remove_server %v, want [memory]", info.Catalogs)
	}
	var listed string
	if err := db.QueryRowContext(t.Context(),
		"SELECT string_agg(catalog_name, ',' ORDER BY catalog_name) FROM n6k_list_catalogs(connection => ?::UUID)", id).Scan(&listed); err != nil {
		t.Fatal(err)
	}
	if listed != "memory" {
		t.Fatalf("connected client lists %q after remove_server, want memory", listed)
	}
}

func TestMCPStopsWithServeClientConnected(t *testing.T) {
	requireExtensions(t)
	configPath := writeMCPConfig(t, nil)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runMCP(ctx, configPath, "127.0.0.1:0", serverTransport, zaptest.NewLogger(t).Sugar())
	}()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()

	var info serveInfo
	if err := json.Unmarshal([]byte(mustCallTool(t, cs, "serve_info", nil)), &info); err != nil {
		t.Fatal(err)
	}
	if _, _, err := serveClient(t, info, info.Token); err != nil {
		t.Fatal(err)
	}

	cancel()
	select {
	case err := <-done:
		if errors.Is(err, context.DeadlineExceeded) || (err != nil && !errors.Is(err, context.Canceled)) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runMCP still running 5s after cancel with a serve client connected")
	}
}

func TestMCPStartupInstructions(t *testing.T) {
	url := startUsersEchoServer(t)
	loginAPIKey(t, url, "s3cret")
	cs, _ := startMCP(t, map[string]string{"users": url})

	want := "Configured n6k servers (connected at startup):\n- users: " + url +
		"\nUse list_servers for live state; add_server and remove_server change it."
	if got := cs.InitializeResult().Instructions; got != want {
		t.Errorf("instructions %q, want %q", got, want)
	}
}
