package perms

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	file, err := Parse([]byte(`
tables:
  app.users:
    key: [id]
    select: true
    insert: true
    update: true
    delete: true
  app.audit_log:
    select: true
  app.orders:
    select: "status <> 'deleted'"
    insert: "amount > 0"
    update: { using: "status = 'open'", check: "amount > 0" }
    delete: "status = 'deleted'"
  app.events:
    insert: true
    update: { check: "amount > 0" }
  app.reporting.events:
    select: true
  foo.bar.baz:
    select: true
`))
	if err != nil {
		t.Fatal(err)
	}
	main := file.Catalogs["app"].Schemas["main"]

	users := main.Tables["users"]
	if !reflect.DeepEqual(users.Key, []string{"id"}) {
		t.Errorf("users key = %v", users.Key)
	}
	if want := []Grant{
		{Select, "true", ""},
		{Insert, "true", "true"},
		{Update, "true", ""},
		{Delete, "true", ""},
	}; !reflect.DeepEqual(users.Grants, want) {
		t.Errorf("users grants = %+v", users.Grants)
	}

	if want := []Grant{{Select, "true", ""}}; !reflect.DeepEqual(main.Tables["audit_log"].Grants, want) {
		t.Errorf("audit_log grants = %+v", main.Tables["audit_log"].Grants)
	}

	if want := []Grant{
		{Select, "status <> 'deleted'", ""},
		{Insert, "true", "amount > 0"},
		{Update, "status = 'open'", "amount > 0"},
		{Delete, "status = 'deleted'", ""},
	}; !reflect.DeepEqual(main.Tables["orders"].Grants, want) {
		t.Errorf("orders grants = %+v", main.Tables["orders"].Grants)
	}

	if want := []Grant{
		{Insert, "true", "true"},
		{Update, "true", "amount > 0"},
	}; !reflect.DeepEqual(main.Tables["events"].Grants, want) {
		t.Errorf("events grants = %+v", main.Tables["events"].Grants)
	}

	if main.Tables["audit_log"].NeedsKey() || !main.Tables["events"].NeedsKey() {
		t.Errorf("NeedsKey: audit_log %v, events %v", main.Tables["audit_log"].NeedsKey(), main.Tables["events"].NeedsKey())
	}
	if _, ok := file.Catalogs["app"].Schemas["reporting"].Tables["events"]; !ok {
		t.Errorf("app.reporting.events missing")
	}
	if _, ok := file.Catalogs["foo"].Schemas["bar"].Tables["baz"]; !ok {
		t.Errorf("foo.bar.baz missing")
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
		{"legacy expose", "expose:\n  app.users: read\n", "legacy perms format"},
		{"legacy catalogs", "version: 1\ncatalogs:\n  app:\n    tables:\n      users: read\n", "legacy perms format"},
		{"empty", "tables: {}\n", "no tables listed"},
		{"bare name", "tables:\n  users:\n    select: true\n", "catalog.table"},
		{"too many parts", "tables:\n  a.b.c.d:\n    select: true\n", "catalog.table"},
		{"reserved catalog", "tables:\n  memory.main.users:\n    select: true\n", "reserved"},
		{"no verbs", "tables:\n  app.users:\n    key: [id]\n", "grants no verbs"},
		{"old access string", "tables:\n  app.users: readwrite\n", "cannot unmarshal"},
		{"unknown verb", "tables:\n  app.users:\n    truncate: true\n", "truncate"},
		{"false", "tables:\n  app.users:\n    select: false\n", "omit the verb"},
		{"null", "tables:\n  app.users:\n    select:\n", "omit the verb"},
		{"empty predicate", "tables:\n  app.users:\n    delete: \" \"\n", "omit the verb"},
		{"select map", "tables:\n  app.users:\n    select: {using: x}\n", "select"},
		{"update string", "tables:\n  app.users:\n    update: \"x > 0\"\n", "update: want true or a map"},
		{"update unknown field", "tables:\n  app.users:\n    update: {where: x}\n", "where"},
		{"update false using", "tables:\n  app.users:\n    update: {using: false}\n", "using"},
		{"empty key", "tables:\n  app.users:\n    key: []\n    select: true\n", "key is empty"},
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
