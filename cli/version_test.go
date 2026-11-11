package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionSkipsBoot(t *testing.T) {
	var out bytes.Buffer
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--version"})
	cmd.SetOut(&out)
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"n6kd ", "duckdb v", "mcp server ", "vcatbridge", "n6k_client", "n6k_server", "quack", "httpfs"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}
