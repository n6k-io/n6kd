// Package perms loads and validates the permissions file that decides which
// tables a client may see, and how.
//
// The file is a flat allowlist keyed by the table's real name on the server:
//
//	expose:
//	  app.main.users:     readwrite
//	  app.main.audit_log: read
//
// Each key is catalog.table (schema defaults to main) or catalog.schema.table,
// naming a table the boot SQL created. It is served under that same name, so a
// client reaches it as <alias>.schema.table after
// ATTACH 'n6k://...' AS <alias> (TYPE n6k, CATALOG 'app'). Anything unlisted
// does not exist for clients.
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

	defaultSchema = "main"
)

// Catalogs a served database already has, so the bridge cannot ATTACH over them.
var reservedCatalogs = map[string]bool{"memory": true, "system": true, "temp": true}

// File is the parsed form the engine consumes.
type File struct {
	// Catalog name -> its schemas. The catalog is both where the table lives on
	// the source and the name a client selects with the CATALOG attach option.
	Catalogs map[string]Catalog
}

type Catalog struct {
	Schemas map[string]Schema
}

type Schema struct {
	// Table name -> Read or ReadWrite.
	Tables map[string]string
	// Overrides for tables whose primary key the bridge cannot discover itself.
	PrimaryKeys map[string][]string
}

// wire is the on-disk shape.
type wire struct {
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
		for _, legacy := range []string{`"source"`, `"catalogs"`, `"version"`} {
			if strings.Contains(err.Error(), legacy) {
				return nil, fmt.Errorf("legacy perms format: the file is now a flat `expose:` map "+
					"of catalog.schema.table -> read|readwrite, naming the table as the server has it (%w)", err)
			}
		}
		return nil, err
	}
	return w.build()
}

func (w wire) build() (*File, error) {
	if len(w.Expose) == 0 {
		return nil, fmt.Errorf("no tables exposed")
	}

	file := &File{Catalogs: map[string]Catalog{}}
	for name, access := range w.Expose {
		catalog, schema, table, err := splitName(name)
		if err != nil {
			return nil, fmt.Errorf("expose %q: %w", name, err)
		}
		if access != Read && access != ReadWrite {
			return nil, fmt.Errorf("expose %q: invalid access %q, want %q or %q", name, access, Read, ReadWrite)
		}
		file.schema(catalog, schema).Tables[table] = access
	}

	for name, columns := range w.Keys {
		catalog, schema, table, err := splitName(name)
		if err != nil {
			return nil, fmt.Errorf("keys %q: %w", name, err)
		}
		entry, ok := file.Catalogs[catalog].Schemas[schema]
		if !ok || entry.Tables[table] == "" {
			return nil, fmt.Errorf("keys names %q, which is not exposed", name)
		}
		if len(columns) == 0 {
			return nil, fmt.Errorf("keys for %q is empty", name)
		}
		entry.PrimaryKeys[table] = columns
	}
	return file, nil
}

func (f *File) schema(catalog, schema string) Schema {
	entry, ok := f.Catalogs[catalog]
	if !ok {
		entry = Catalog{Schemas: map[string]Schema{}}
		f.Catalogs[catalog] = entry
	}
	tables, ok := entry.Schemas[schema]
	if !ok {
		tables = Schema{Tables: map[string]string{}, PrimaryKeys: map[string][]string{}}
		entry.Schemas[schema] = tables
	}
	return tables
}

func splitName(name string) (catalog, schema, table string, err error) {
	parts := strings.Split(name, ".")
	if slices.Contains(parts, "") {
		return "", "", "", fmt.Errorf("empty name component")
	}
	switch len(parts) {
	case 2:
		catalog, schema, table = parts[0], defaultSchema, parts[1]
	case 3:
		catalog, schema, table = parts[0], parts[1], parts[2]
	default:
		return "", "", "", fmt.Errorf("want catalog.table or catalog.schema.table")
	}
	if reservedCatalogs[strings.ToLower(catalog)] {
		return "", "", "", fmt.Errorf("catalog %q is reserved; ATTACH the schema under a name of its own", catalog)
	}
	return catalog, schema, table, nil
}
