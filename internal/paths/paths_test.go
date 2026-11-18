package paths

import (
	"path/filepath"
	"testing"
)

func TestConfigDirXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	got, err := ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/xdg", "n6kd"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestConfigDirHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/u")
	got, err := ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/home/u", ".config", "n6kd"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
