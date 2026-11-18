package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigMissing(t *testing.T) {
	c, err := loadConfig(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.extensionVersions()) != 0 {
		t.Errorf("got versions %v", c.extensionVersions())
	}
}

func TestLoadConfigVersions(t *testing.T) {
	c, err := loadConfig(writeConfig(t, "version: 1\nextensions:\n  vcatbridge:\n    version: v0\n  n6k_client: {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := c.extensionVersions()
	if got["vcatbridge"] != "v0" || got["n6k_client"] != "" || got["n6k_server"] != "" {
		t.Errorf("got %v", got)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"missing version": {"extensions: {}\n", "version must be 1"},
		"wrong version":   {"version: 2\n", "version must be 1"},
		"unknown ext":     {"version: 1\nextensions:\n  httpfs:\n    version: v1\n", "extensions.httpfs"},
		"unknown key":     {"version: 1\nversions: {}\n", "unknown field"},
		"unknown ext key": {"version: 1\nextensions:\n  vcatbridge:\n    repo: x\n", "unknown field"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadConfig(writeConfig(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestDefaultConfigPath(t *testing.T) {
	t.Setenv("N6KD_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got, want := defaultConfigPath(), filepath.Join("/xdg", "n6kd", "config.yaml"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	t.Setenv("N6KD_CONFIG", "/etc/n6kd.yaml")
	if got := defaultConfigPath(); got != "/etc/n6kd.yaml" {
		t.Errorf("got %q", got)
	}
}
