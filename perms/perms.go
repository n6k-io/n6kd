// Package perms loads the permissions file that decides which catalogs a client
// may see, and how.
//
// The file maps each served catalog to a virtual_catalog_bridge policy
// document, passed to bridge_configure_policy as written:
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
package perms

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

const policyVersion = 1

// Catalogs a served database already has, so the bridge cannot ATTACH over them.
var reservedCatalogs = map[string]bool{"memory": true, "system": true, "temp": true}

// File is the parsed form the engine consumes.
type File struct {
	// Catalog name -> its bridge_configure_policy document.
	Catalogs map[string]json.RawMessage
}

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
	// Strict so a typo'd key is an error rather than a silently ignored setting.
	if err := yaml.UnmarshalStrict(raw, &w); err != nil {
		return nil, err
	}
	if len(w.Catalogs) == 0 {
		return nil, fmt.Errorf("no catalogs listed")
	}

	file := &File{Catalogs: map[string]json.RawMessage{}}
	for name, policy := range w.Catalogs {
		if reservedCatalogs[strings.ToLower(name)] {
			return nil, fmt.Errorf("catalogs %q: reserved; ATTACH the schema under a name of its own", name)
		}
		if policy == nil {
			return nil, fmt.Errorf("catalogs %q: want a policy document", name)
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
