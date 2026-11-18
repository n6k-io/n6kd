package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"sigs.k8s.io/yaml"

	"github.com/n6k-io/n6kd/engine"
	"github.com/n6k-io/n6kd/internal/paths"
)

const configVersion = 1

type n6kdConfig struct {
	Version    int                        `json:"version"`
	Extensions map[string]extensionConfig `json:"extensions,omitempty"`
}

type extensionConfig struct {
	Version string `json:"version,omitempty"`
}

func defaultConfigPath() string {
	if path := os.Getenv("N6KD_CONFIG"); path != "" {
		return path
	}
	dir, err := paths.ConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "config.yaml")
}

func loadConfig(path string) (n6kdConfig, error) {
	var c n6kdConfig
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
	if c.Version != configVersion {
		return c, fmt.Errorf("%s: version must be %d, got %d", path, configVersion, c.Version)
	}
	known := engine.VersionedExtensions()
	for name := range c.Extensions {
		if !slices.Contains(known, name) {
			return c, fmt.Errorf("%s: extensions.%s: not one of %v", path, name, known)
		}
	}
	return c, nil
}

func (c n6kdConfig) extensionVersions() map[string]string {
	versions := map[string]string{}
	for name, e := range c.Extensions {
		versions[name] = e.Version
	}
	return versions
}
