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
	for _, path := range []string{"../examples/perms.yml", "../examples/server/perms.yml"} {
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
