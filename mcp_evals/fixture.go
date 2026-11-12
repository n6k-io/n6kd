//go:build eval

package mcp_evals

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
	"sigs.k8s.io/yaml"

	"github.com/n6k-io/n6kd/engine"
	"github.com/n6k-io/n6kd/server"
)

func RequireExtensions(t *testing.T) {
	t.Helper()
	if os.Getenv(engine.ExtensionDirEnv) == "" {
		t.Skipf("%s not set", engine.ExtensionDirEnv)
	}
}

func BuildN6kd(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "n6kd")
	// #nosec G204 -- bin is a path under t.TempDir.
	out, err := exec.CommandContext(t.Context(), "go", "build", "-o", bin, "github.com/n6k-io/n6kd").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// N6kdMCP runs `n6kd mcp` with servers in its config and a fresh HOME, so no logins are stored.
func N6kdMCP(t *testing.T, bin string, servers map[string]string) MCPServer {
	t.Helper()
	entries := map[string]map[string]string{}
	for name, url := range servers {
		entries[name] = map[string]string{"url": url}
	}
	raw, err := yaml.Marshal(map[string]any{"servers": entries})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "mcp.yml")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return MCPServer{
		Command: bin,
		Args:    []string{"mcp", "--config", configPath},
		Env:     map[string]string{"HOME": t.TempDir()},
	}
}

func StartServer(t *testing.T, opts engine.Options) string {
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

func BootSQL(t *testing.T, sql string) []engine.Step {
	t.Helper()
	path := filepath.Join(t.TempDir(), "boot.sql")
	if err := os.WriteFile(path, []byte(sql), 0o600); err != nil {
		t.Fatal(err)
	}
	return []engine.Step{{Path: path}}
}
