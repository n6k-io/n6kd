// Package perms loads and validates the permissions file that decides which
// tables a client may see, and how.
//
// The file maps each table, by its real name on the server, to the verbs it
// grants. Each verb mirrors a virtual_catalog_bridge policy:
//
//	tables:
//	  app.main.users:
//	    key: [id]
//	    select: true
//	    insert: true
//	    update: true
//	    delete: true
//	  app.main.orders:
//	    select: "status <> 'deleted'"
//	    insert: "amount > 0"
//	    update: { using: "status = 'open'", check: "amount > 0" }
//
// Each key is catalog.table (schema defaults to main) or catalog.schema.table,
// naming a table the boot SQL created. It is served under that same name, so a
// client reaches it as <alias>.schema.table after
// ATTACH 'n6k://...' AS <alias> (TYPE n6k, CATALOG 'app'). Anything unlisted
// does not exist for clients, and an unlisted verb is denied.
//
// A verb is true or a predicate. select and delete take a USING predicate,
// insert a WITH CHECK predicate, and update a map of either or both; this is
// all the bridge accepts. update and delete need a primary key, which the engine
// resolves at boot, where a config mistake belongs.
package perms

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

const (
	Select = "select"
	Insert = "insert"
	Update = "update"
	Delete = "delete"

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
	Tables map[string]Table
}

type Table struct {
	// Override for a table whose primary key the bridge cannot discover itself.
	Key    []string
	Grants []Grant
}

type Grant struct {
	Verb, Using, Check string
}

func (t Table) NeedsKey() bool {
	return slices.ContainsFunc(t.Grants, func(g Grant) bool {
		return g.Verb == Update || g.Verb == Delete
	})
}

func (t Table) Verbs() []string {
	verbs := make([]string, len(t.Grants))
	for i, g := range t.Grants {
		verbs[i] = g.Verb
	}
	return verbs
}

// wire is the on-disk shape.
type wire struct {
	Tables map[string]wireTable `json:"tables"`
}

type wireTable struct {
	Key    []string        `json:"key"`
	Select json.RawMessage `json:"select"`
	Insert json.RawMessage `json:"insert"`
	Update json.RawMessage `json:"update"`
	Delete json.RawMessage `json:"delete"`
}

type wireUpdate struct {
	Using json.RawMessage `json:"using"`
	Check json.RawMessage `json:"check"`
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
		for _, legacy := range []string{`"expose"`, `"keys"`, `"source"`, `"catalogs"`, `"version"`} {
			if strings.Contains(err.Error(), legacy) {
				return nil, fmt.Errorf("legacy perms format: the file is now a `tables:` map "+
					"of catalog.schema.table -> verbs (select, insert, update, delete) and an optional key (%w)", err)
			}
		}
		return nil, err
	}
	return w.build()
}

func (w wire) build() (*File, error) {
	if len(w.Tables) == 0 {
		return nil, fmt.Errorf("no tables listed")
	}

	file := &File{Catalogs: map[string]Catalog{}}
	for name, wt := range w.Tables {
		catalog, schema, table, err := splitName(name)
		if err != nil {
			return nil, fmt.Errorf("tables %q: %w", name, err)
		}
		t, err := wt.build()
		if err != nil {
			return nil, fmt.Errorf("tables %q: %w", name, err)
		}
		file.schema(catalog, schema).Tables[table] = t
	}
	return file, nil
}

func (wt wireTable) build() (Table, error) {
	var t Table
	if wt.Key != nil && len(wt.Key) == 0 {
		return t, fmt.Errorf("key is empty")
	}
	t.Key = wt.Key

	if wt.Select != nil {
		using, err := predicate(wt.Select)
		if err != nil {
			return t, fmt.Errorf("select: %w", err)
		}
		t.Grants = append(t.Grants, Grant{Verb: Select, Using: using})
	}
	if wt.Insert != nil {
		check, err := predicate(wt.Insert)
		if err != nil {
			return t, fmt.Errorf("insert: %w", err)
		}
		t.Grants = append(t.Grants, Grant{Verb: Insert, Using: "true", Check: check})
	}
	if wt.Update != nil {
		g, err := updateGrant(wt.Update)
		if err != nil {
			return t, fmt.Errorf("update: %w", err)
		}
		t.Grants = append(t.Grants, g)
	}
	if wt.Delete != nil {
		using, err := predicate(wt.Delete)
		if err != nil {
			return t, fmt.Errorf("delete: %w", err)
		}
		t.Grants = append(t.Grants, Grant{Verb: Delete, Using: using})
	}

	if len(t.Grants) == 0 {
		return t, fmt.Errorf("grants no verbs; list at least one of select, insert, update, delete")
	}
	return t, nil
}

func updateGrant(raw json.RawMessage) (Grant, error) {
	g := Grant{Verb: Update, Using: "true"}
	switch string(raw) {
	case "true":
		return g, nil
	case "null":
		return g, fmt.Errorf("want true or a map of using and/or check; omit the verb to deny it")
	}
	var wu wireUpdate
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wu); err != nil {
		return g, fmt.Errorf("want true or a map of using and/or check (%w)", err)
	}
	if wu.Using != nil {
		using, err := predicate(wu.Using)
		if err != nil {
			return g, fmt.Errorf("using: %w", err)
		}
		g.Using = using
	}
	if wu.Check != nil {
		check, err := predicate(wu.Check)
		if err != nil {
			return g, fmt.Errorf("check: %w", err)
		}
		g.Check = check
	}
	return g, nil
}

func predicate(raw json.RawMessage) (string, error) {
	if string(raw) == "true" {
		return "true", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("want true or a SQL predicate, got %s; omit the verb to deny it", raw)
	}
	return s, nil
}

func (f *File) schema(catalog, schema string) Schema {
	entry, ok := f.Catalogs[catalog]
	if !ok {
		entry = Catalog{Schemas: map[string]Schema{}}
		f.Catalogs[catalog] = entry
	}
	tables, ok := entry.Schemas[schema]
	if !ok {
		tables = Schema{Tables: map[string]Table{}}
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
