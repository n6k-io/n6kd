package perms

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	file, err := Parse([]byte(`
catalogs:
  app:
    tables:
      main.users:
        primary_key: [id]
        select: true
      main.orders:
        update: { using: "status = 'open'", check: "amount > 0" }
    functions: { add: [my_udf] }
  foo:
    policy_version: 2
    tables:
      bar.baz:
        select: true
`))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"app": `{"functions":{"add":["my_udf"]},"policy_version":1,"tables":{"main.orders":{"update":{"check":"amount \u003e 0","using":"status = 'open'"}},"main.users":{"primary_key":["id"],"select":true}}}`,
		"foo": `{"policy_version":2,"tables":{"bar.baz":{"select":true}}}`,
	} {
		var got, wantDoc any
		if err := json.Unmarshal(file.Catalogs[name], &got); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := json.Unmarshal([]byte(want), &wantDoc); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, wantDoc) {
			t.Errorf("%s = %s", name, file.Catalogs[name])
		}
	}
}

func TestExamplesParse(t *testing.T) {
	for _, path := range []string{"../examples/perms.yml", "../examples/server/perms.yml", "../examples/multi-source/perms.yml"} {
		if _, err := Load(path); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"empty", "catalogs: {}\n", "no catalogs listed"},
		{"old tables", "tables:\n  app.users:\n    select: true\n", "tables"},
		{"reserved catalog", "catalogs:\n  memory:\n    tables: {}\n", "reserved"},
		{"null policy", "catalogs:\n  app:\n", "want a policy document"},
		{"policy not a map", "catalogs:\n  app: true\n", "cannot unmarshal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestParseDocs(t *testing.T) {
	file, err := Parse([]byte(`
catalogs:
  app:
    description: "Orders"
    hints: "Amounts are in cents."
    prompts:
      open_orders:
        description: "List open orders"
        args:
          customer: { description: "One customer" }
          limit: { required: true }
        template: "Top {{.limit}} open orders{{if .customer}} for {{.customer}}{{end}}."
    tables:
      main.orders:
        select: true
        description: "One row per order"
        hints: "Never delete"
        columns: { status: "open | shipped" }
        examples: ["SELECT 1"]
      main.users:
        select: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(file.Catalogs["app"]), `{"policy_version":1,"tables":{"main.orders":{"select":true},"main.users":{"select":true}}}`; got != want {
		t.Errorf("policy %s, want %s", got, want)
	}
	docs := file.Docs["app"]
	want := Catalog{
		Description: "Orders",
		Hints:       "Amounts are in cents.",
		Tables: map[string]Table{"main.orders": {
			Description: "One row per order", Hints: "Never delete",
			Columns: map[string]string{"status": "open | shipped"}, Examples: []string{"SELECT 1"},
		}},
		Prompts: map[string]Prompt{"open_orders": {
			Description: "List open orders",
			Args:        map[string]Arg{"customer": {Description: "One customer"}, "limit": {Required: true}},
			Template:    "Top {{.limit}} open orders{{if .customer}} for {{.customer}}{{end}}.",
		}},
	}
	if !reflect.DeepEqual(docs, want) {
		t.Errorf("docs %+v", docs)
	}

	prompt := docs.Prompts["open_orders"]
	for _, tc := range []struct {
		args       map[string]string
		want, fail string
	}{
		{map[string]string{"limit": "5"}, "Top 5 open orders.", ""},
		{map[string]string{"limit": "5", "customer": "bob"}, "Top 5 open orders for bob.", ""},
		{nil, "", `argument "limit" is required`},
		{map[string]string{"limit": "5", "other": "x"}, "", `argument "other" is not declared`},
	} {
		got, err := prompt.Render(tc.args)
		if tc.fail != "" {
			if err == nil || !strings.Contains(err.Error(), tc.fail) {
				t.Errorf("%v: got %q, %v, want error %q", tc.args, got, err, tc.fail)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%v: got %q, %v, want %q", tc.args, got, err, tc.want)
		}
	}
}

func TestParseWithoutDocs(t *testing.T) {
	file, err := Parse([]byte("catalogs:\n  app:\n    tables:\n      main.users:\n        select: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Docs) != 0 {
		t.Errorf("docs %v", file.Docs)
	}
}

func TestParseDocsErrors(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"missing template", "catalogs:\n  app:\n    prompts:\n      p: { description: x }\n", "template is required"},
		{"undeclared arg", "catalogs:\n  app:\n    prompts:\n      p: { template: '{{.who}}' }\n", `map has no entry for key "who"`},
		{"bad template", "catalogs:\n  app:\n    prompts:\n      p: { template: '{{' }\n", "unclosed action"},
		{"unknown prompt key", "catalogs:\n  app:\n    prompts:\n      p: { template: x, nope: 1 }\n", `unknown field "nope"`},
		{"columns not strings", "catalogs:\n  app:\n    tables:\n      main.t: { select: true, columns: [a] }\n", "cannot unmarshal"},
		{"duplicate prompt name", "catalogs:\n  a_b:\n    prompts:\n      c: { template: x }\n  a:\n    prompts:\n      b_c: { template: x }\n", `served as "a_b_c"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want error containing %q", err, tc.want)
			}
		})
	}
}
