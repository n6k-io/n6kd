package engine

import "testing"

func TestInstallStatement(t *testing.T) {
	for _, tc := range []struct {
		name, repo, version string
		force               bool
		want                string
	}{
		{"httpfs", "", "", false, "INSTALL 'httpfs'"},
		{"httpfs", "", "", true, "FORCE INSTALL 'httpfs'"},
		{"vcatbridge", "https://r", "", false, "INSTALL 'vcatbridge' FROM 'https://r'"},
		{"vcatbridge", "https://r", "", true, "FORCE INSTALL 'vcatbridge' FROM 'https://r'"},
		{"vcatbridge", "https://r", "v0", false, "INSTALL 'vcatbridge' FROM 'https://r' VERSION 'v0'"},
		{"vcatbridge", "https://r", "v0", true, "FORCE INSTALL 'vcatbridge' FROM 'https://r' VERSION 'v0'"},
		{"vcatbridge", "https://r'x", "v'0", false, "INSTALL 'vcatbridge' FROM 'https://r''x' VERSION 'v''0'"},
	} {
		if got := installStatement(tc.name, tc.repo, tc.version, tc.force); got != tc.want {
			t.Errorf("installStatement(%q, %q, %q, %v) = %q, want %q", tc.name, tc.repo, tc.version, tc.force, got, tc.want)
		}
	}
}

func TestVersionMatches(t *testing.T) {
	for _, tc := range []struct {
		installed, pin string
		want           bool
	}{
		{"v0.2.5", "", true},
		{"v0.2.5", "v0.2.5", true},
		{"v0.2.5", "v0.2", true},
		{"v0.2.5", "v0", true},
		{"v0.2.5", "v0.2.4", false},
		{"v0.2.5", "v0.20", false},
		{"v0.2.5", "v1", false},
		{"", "v0", false},
	} {
		if got := versionMatches(tc.installed, tc.pin); got != tc.want {
			t.Errorf("versionMatches(%q, %q) = %v, want %v", tc.installed, tc.pin, got, tc.want)
		}
	}
}
