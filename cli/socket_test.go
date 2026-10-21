package cli

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestListenRefusesAuthFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--api-key", "k"},
		{"--oidc-issuer-url", "https://issuer.example", "--oidc-audience", "a"},
	} {
		err := runRoot(t, append([]string{"--listen", filepath.Join(t.TempDir(), "db.sock")}, args...))
		if err == nil || !strings.Contains(err.Error(), "cannot be used with --listen") {
			t.Errorf("%v: got %v, want the --listen contradiction", args, err)
		}
	}
}

func runRoot(t *testing.T, args []string) error {
	t.Helper()
	cmd := newRootCmd()
	cmd.SetArgs(append([]string{"-c", "SELECT 1"}, args...))
	cmd.SetErr(io.Discard)
	cmd.SetOut(io.Discard)
	return cmd.ExecuteContext(t.Context())
}
