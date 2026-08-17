// Package perms loads and validates the permissions file that decides which
// tables a client may see, and how.
//
// The rules enforced here are the ones src/n6k_bridge/bridge_bridge.cpp throws
// on: bare table names, read/readwrite only, no empty table set, and a primary
// key for anything writable. Checking them at startup turns a per-connection
// error into a failure to boot, which is where a config mistake belongs.
package perms

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

const (
	Read      = "read"
	ReadWrite = "readwrite"

	defaultSourceCatalog = "memory"
	defaultSchema        = "main"
)

type File struct {
	Version  int                `json:"version"`
	Source   Source             `json:"source"`
	Catalogs map[string]Catalog `json:"catalogs"`
}

// Source locates the tables the schema file created. A bare CREATE TABLE in
// foo.sql lands in memory.main, which is why those are the defaults.
type Source struct {
	Catalog string `json:"catalog"`
	Schema  string `json:"schema"`
}

type Catalog struct {
	Schema string `json:"schema"`
	// Table name -> Read or ReadWrite. Bare names only; the bridge rejects dots.
	Tables map[string]string `json:"tables"`
	// Overrides for tables whose primary key the bridge cannot discover itself.
	PrimaryKeys map[string][]string `json:"primary_keys"`
}

func Load(path string) (*File, error) {
	// #nosec G304 -- the operator names this file on the command line.
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file File
	// Strict so a typo'd key is an error rather than a silently ignored setting.
	if err := yaml.UnmarshalStrict(raw, &file); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	file.applyDefaults()
	if err := file.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &file, nil
}

func (f *File) applyDefaults() {
	if f.Source.Catalog == "" {
		f.Source.Catalog = defaultSourceCatalog
	}
	if f.Source.Schema == "" {
		f.Source.Schema = defaultSchema
	}
	for name, catalog := range f.Catalogs {
		if catalog.Schema == "" {
			catalog.Schema = defaultSchema
			f.Catalogs[name] = catalog
		}
	}
}

func (f *File) validate() error {
	if f.Version != 1 {
		return fmt.Errorf("unsupported version %d, want 1", f.Version)
	}
	if len(f.Catalogs) == 0 {
		return fmt.Errorf("no catalogs defined")
	}
	for name, catalog := range f.Catalogs {
		if err := catalog.validate(); err != nil {
			return fmt.Errorf("catalog %q: %w", name, err)
		}
	}
	return nil
}

func (c Catalog) validate() error {
	if len(c.Tables) == 0 {
		return fmt.Errorf("no tables defined")
	}
	for table, access := range c.Tables {
		if table == "" {
			return fmt.Errorf("empty table name")
		}
		if strings.Contains(table, ".") {
			return fmt.Errorf("table %q must be a bare name, without a schema or catalog prefix", table)
		}
		if access != Read && access != ReadWrite {
			return fmt.Errorf("table %q: invalid access %q, want %q or %q", table, access, Read, ReadWrite)
		}
	}
	for table, columns := range c.PrimaryKeys {
		if _, ok := c.Tables[table]; !ok {
			return fmt.Errorf("primary_keys names %q, which is not in tables", table)
		}
		if len(columns) == 0 {
			return fmt.Errorf("primary_keys for %q is empty", table)
		}
	}
	return nil
}
