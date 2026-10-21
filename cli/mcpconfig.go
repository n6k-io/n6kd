package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"

	"sigs.k8s.io/yaml"
)

type mcpConfig struct {
	Servers map[string]mcpServerEntry `json:"servers"`
}

type mcpServerEntry struct {
	URL string `json:"url"`
}

var serverNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func defaultMCPConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".n6kd", "mcp.yml")
}

func loadMCPConfig(path string) (mcpConfig, error) {
	c := mcpConfig{Servers: map[string]mcpServerEntry{}}
	// #nosec G304 -- the operator names this file with --config.
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := yaml.UnmarshalStrict(raw, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.Servers == nil {
		c.Servers = map[string]mcpServerEntry{}
	}
	for name, entry := range c.Servers {
		if err := validateServer(name, entry.URL); err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
	}
	return c, nil
}

func saveMCPConfig(path string, c mcpConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func validateServer(name, rawURL string) error {
	if !serverNamePattern.MatchString(name) {
		return fmt.Errorf("server name %q must match %s", name, serverNamePattern)
	}
	_, err := serverScope(rawURL)
	return err
}

func serverScope(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("server url: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("server url %q has no host", rawURL)
	}
	return u.Host, nil
}
