// Package perms loads and validates the permissions file that decides which
// tables a client may see, and how.
//
// The file is a flat allowlist, mirroring the Python bridge's expose={...}:
//
//	expose:
//	  app.users:     readwrite
//	  app.audit_log: read
//
// Each key is catalog.table (or catalog.schema.table); the catalog part is the
// name a client ATTACHes. Anything unlisted does not exist for clients.
//
// The rules enforced here are the ones src/virtual_catalog/bridge_bridge.cpp throws
// on: read/readwrite only, no empty expose set, and a primary key for anything
// writable. Checking them at startup turns a per-connection error into a
// failure to boot, which is where a config mistake belongs.
package perms

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

const (
	Read      = "read"
	ReadWrite = "readwrite"

	defaultSourceCatalog = "memory"
	defaultSchema        = "main"
)

// File is the parsed form the engine consumes.
type File struct {
	Source   Source
	Catalogs map[string]Catalog
}

// Source locates the tables the schema file created. A bare CREATE TABLE in
// schema.sql lands in memory.main, which is why those are the defaults.
type Source struct {
	Catalog string
	Schema  string
}

type Catalog struct {
	Schema string
	// Table name -> Read or ReadWrite.
	Tables map[string]string
	// Overrides for tables whose primary key the bridge cannot discover itself.
	PrimaryKeys map[string][]string
}

// wire is the on-disk shape.
type wire struct {
	// "catalog.schema" naming where schema.sql's tables landed; default memory.main.
	Source string `json:"source"`
	// "catalog.table" or "catalog.schema.table" -> read | readwrite.
	Expose map[string]string `json:"expose"`
	// Same dotted names -> primary key columns, only where discovery can't.
	Keys map[string][]string `json:"keys"`
}

func Load(path string) (*File, error) {
	// #nosec G304 -- the operator names this file on the command line.
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	file, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return file, nil
}

func Parse(raw []byte) (*File, error) {
	var w wire
	// Strict so a typo'd key is an error rather than a silently ignored setting.
	if err := yaml.UnmarshalStrict(raw, &w); err != nil {
		if strings.Contains(err.Error(), `"catalogs"`) || strings.Contains(err.Error(), `"version"`) {
			return nil, fmt.Errorf("legacy perms format: the file is now a flat `expose:` map of catalog.table -> read|readwrite (%w)", err)
		}
		return nil, err
	}
	return w.build()
}

func (w wire) build() (*File, error) {
	source, err := parseSource(w.Source)
	if err != nil {
		return nil, err
	}
	if len(w.Expose) == 0 {
		return nil, fmt.Errorf("no tables exposed")
	}

	file := &File{Source: source, Catalogs: map[string]Catalog{}}
	for name, access := range w.Expose {
		catalog, schema, table, err := splitName(name)
		if err != nil {
			return nil, fmt.Errorf("expose %q: %w", name, err)
		}
		if access != Read && access != ReadWrite {
			return nil, fmt.Errorf("expose %q: invalid access %q, want %q or %q", name, access, Read, ReadWrite)
		}
		entry, ok := file.Catalogs[catalog]
		if !ok {
			entry = Catalog{Schema: schema, Tables: map[string]string{}, PrimaryKeys: map[string][]string{}}
		} else if entry.Schema != schema {
			return nil, fmt.Errorf("catalog %q exposed under two schemas, %q and %q", catalog, entry.Schema, schema)
		}
		entry.Tables[table] = access
		file.Catalogs[catalog] = entry
	}

	for name, columns := range w.Keys {
		catalog, schema, table, err := splitName(name)
		if err != nil {
			return nil, fmt.Errorf("keys %q: %w", name, err)
		}
		entry, ok := file.Catalogs[catalog]
		if !ok || entry.Schema != schema || entry.Tables[table] == "" {
			return nil, fmt.Errorf("keys names %q, which is not exposed", name)
		}
		if len(columns) == 0 {
			return nil, fmt.Errorf("keys for %q is empty", name)
		}
		entry.PrimaryKeys[table] = columns
	}
	return file, nil
}

func parseSource(s string) (Source, error) {
	if s == "" {
		return Source{Catalog: defaultSourceCatalog, Schema: defaultSchema}, nil
	}
	parts := strings.Split(s, ".")
	switch len(parts) {
	case 1:
		return Source{Catalog: parts[0], Schema: defaultSchema}, nil
	case 2:
		return Source{Catalog: parts[0], Schema: parts[1]}, nil
	}
	return Source{}, fmt.Errorf("source %q: want catalog or catalog.schema", s)
}

func splitName(name string) (catalog, schema, table string, err error) {
	parts := strings.Split(name, ".")
	if slices.Contains(parts, "") {
		return "", "", "", fmt.Errorf("empty name component")
	}
	switch len(parts) {
	case 2:
		return parts[0], defaultSchema, parts[1], nil
	case 3:
		return parts[0], parts[1], parts[2], nil
	}
	return "", "", "", fmt.Errorf("want catalog.table or catalog.schema.table")
}
