// Package perms loads the permissions file that decides which catalogs a client
// may see, and how.
//
// The file maps each served catalog to a vcatbridge policy
// document, passed to vcatbridge_configure_policy as written:
//
//	catalogs:
//	  app:
//	    tables:
//	      main.users:
//	        primary_key: [id]
//	        select: true
//	        insert: true
//	        update: true
//	        delete: true
//	      main.orders:
//	        select: "status <> 'deleted'"
//	        insert: "amount > 0"
//	        update: { using: "status = 'open'", check: "amount > 0" }
//
// The catalog is both where the tables live on the source and the name a client
// selects with ATTACH 'n6k://...' AS <alias> (TYPE n6k, CATALOG 'app').
// policy_version defaults to 1. The bridge validates the document; see
// duckdb-virtual-catalog docs/policy-v1.schema.json.
//
// Docs for MCP clients sit inline and are removed before the policy reaches the
// bridge: description, hints and prompts on a catalog; description, hints,
// columns and examples on a table.
//
//	catalogs:
//	  app:
//	    description: "Orders and their customers"
//	    hints: "Amounts are in cents."
//	    prompts:
//	      open_orders:
//	        description: "List open orders"
//	        args:
//	          customer: { description: "Limit to one customer" }
//	        template: "List open orders{{if .customer}} for {{.customer}}{{end}}."
//	    tables:
//	      main.orders:
//	        select: true
//	        description: "One row per order"
//	        columns: { status: "open | shipped | cancelled" }
//	        examples: ["SELECT * FROM app.main.orders WHERE status = 'open'"]
package perms

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"text/template"

	"sigs.k8s.io/yaml"
)

const policyVersion = 1

// Catalogs a served database already has, so the bridge cannot ATTACH over them.
var reservedCatalogs = map[string]bool{"memory": true, "system": true, "temp": true}

type File struct {
	// Catalog name -> its vcatbridge_configure_policy document.
	Catalogs map[string]json.RawMessage
	// Catalog name -> its docs; only catalogs that have any.
	Docs map[string]Catalog
}

type Catalog struct {
	Description string            `json:"description,omitempty"`
	Hints       string            `json:"hints,omitempty"`
	Tables      map[string]Table  `json:"tables,omitempty"`
	Prompts     map[string]Prompt `json:"prompts,omitempty"`
}

type Table struct {
	Description string            `json:"description,omitempty"`
	Hints       string            `json:"hints,omitempty"`
	Columns     map[string]string `json:"columns,omitempty"`
	Examples    []string          `json:"examples,omitempty"`
}

type Prompt struct {
	Description string         `json:"description,omitempty"`
	Args        map[string]Arg `json:"args,omitempty"`
	Template    string         `json:"template"`
}

type Arg struct {
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

var (
	catalogDocKeys = []string{"description", "hints", "prompts"}
	tableDocKeys   = []string{"description", "hints", "columns", "examples"}
)

type wire struct {
	Catalogs map[string]map[string]json.RawMessage `json:"catalogs"`
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
	if err := yaml.UnmarshalStrict(raw, &w); err != nil {
		return nil, err
	}
	if len(w.Catalogs) == 0 {
		return nil, fmt.Errorf("no catalogs listed")
	}

	file := &File{Catalogs: map[string]json.RawMessage{}, Docs: map[string]Catalog{}}
	promptOwners := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(w.Catalogs)) {
		policy := w.Catalogs[name]
		if reservedCatalogs[strings.ToLower(name)] {
			return nil, fmt.Errorf("catalogs %q: reserved; ATTACH the schema under a name of its own", name)
		}
		if policy == nil {
			return nil, fmt.Errorf("catalogs %q: want a policy document", name)
		}
		docs, err := splitDocs(policy)
		if err != nil {
			return nil, fmt.Errorf("catalogs %q: %w", name, err)
		}
		for _, prompt := range slices.Sorted(maps.Keys(docs.Prompts)) {
			full := name + "_" + prompt
			if other, taken := promptOwners[full]; taken {
				return nil, fmt.Errorf("catalogs %q prompts %q: served as %q, as is a prompt of catalog %q", name, prompt, full, other)
			}
			promptOwners[full] = name
		}
		if docs.Description != "" || docs.Hints != "" || len(docs.Tables) > 0 || len(docs.Prompts) > 0 {
			file.Docs[name] = docs
		}

		if _, ok := policy["policy_version"]; !ok {
			policy["policy_version"] = json.RawMessage(fmt.Sprint(policyVersion))
		}
		doc, err := json.Marshal(policy)
		if err != nil {
			return nil, fmt.Errorf("catalogs %q: %w", name, err)
		}
		file.Catalogs[name] = doc
	}
	return file, nil
}

// splitDocs removes the doc keys from policy and returns them.
func splitDocs(policy map[string]json.RawMessage) (Catalog, error) {
	var docs Catalog
	if err := decodeKeys(policy, catalogDocKeys, &docs); err != nil {
		return Catalog{}, err
	}
	for _, name := range slices.Sorted(maps.Keys(docs.Prompts)) {
		if err := docs.Prompts[name].check(); err != nil {
			return Catalog{}, fmt.Errorf("prompts %q: %w", name, err)
		}
	}

	raw, ok := policy["tables"]
	if !ok {
		return docs, nil
	}
	var tables map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tables); err != nil {
		return Catalog{}, fmt.Errorf("tables: %w", err)
	}
	for name, table := range tables {
		var t Table
		if err := decodeKeys(table, tableDocKeys, &t); err != nil {
			return Catalog{}, fmt.Errorf("tables %q: %w", name, err)
		}
		if t.Description != "" || t.Hints != "" || len(t.Columns) > 0 || len(t.Examples) > 0 {
			if docs.Tables == nil {
				docs.Tables = map[string]Table{}
			}
			docs.Tables[name] = t
		}
	}
	stripped, err := json.Marshal(tables)
	if err != nil {
		return Catalog{}, fmt.Errorf("tables: %w", err)
	}
	policy["tables"] = stripped
	return docs, nil
}

// decodeKeys moves keys out of from and strictly decodes them into into.
func decodeKeys(from map[string]json.RawMessage, keys []string, into any) error {
	picked := map[string]json.RawMessage{}
	for _, key := range keys {
		if v, ok := from[key]; ok {
			picked[key] = v
			delete(from, key)
		}
	}
	if len(picked) == 0 {
		return nil
	}
	raw, err := json.Marshal(picked)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

func (p Prompt) check() error {
	if strings.TrimSpace(p.Template) == "" {
		return fmt.Errorf("template is required")
	}
	data := map[string]string{}
	for name := range p.Args {
		data[name] = ""
	}
	_, err := render(p.Template, data)
	return err
}

// Render fills the template from args. Declared args left out render as "".
func (p Prompt) Render(args map[string]string) (string, error) {
	for _, name := range slices.Sorted(maps.Keys(args)) {
		if _, ok := p.Args[name]; !ok {
			return "", fmt.Errorf("argument %q is not declared", name)
		}
	}
	data := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(p.Args)) {
		v, ok := args[name]
		if !ok && p.Args[name].Required {
			return "", fmt.Errorf("argument %q is required", name)
		}
		data[name] = v
	}
	return render(p.Template, data)
}

func render(text string, data map[string]string) (string, error) {
	tmpl, err := template.New("prompt").Option("missingkey=error").Parse(text)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}
