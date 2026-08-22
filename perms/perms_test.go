package perms

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	file, err := Parse([]byte(`
expose:
  app.users: readwrite
  app.audit_log: read
  app.reporting.events: read
  foo.bar.baz: read
keys:
  app.users: [id]
`))
	if err != nil {
		t.Fatal(err)
	}
	main := file.Catalogs["app"].Schemas["main"]
	if main.Tables["users"] != ReadWrite || main.Tables["audit_log"] != Read {
		t.Errorf("app.main = %+v", main)
	}
	if got := main.PrimaryKeys["users"]; len(got) != 1 || got[0] != "id" {
		t.Errorf("app.main keys = %v", got)
	}
	if reporting := file.Catalogs["app"].Schemas["reporting"]; reporting.Tables["events"] != Read {
		t.Errorf("app.reporting = %+v", reporting)
	}
	if foo := file.Catalogs["foo"].Schemas["bar"]; foo.Tables["baz"] != Read {
		t.Errorf("foo.bar = %+v", foo)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"legacy format", "version: 1\ncatalogs:\n  app:\n    tables:\n      users: read\n", "legacy perms format"},
		{"legacy source", "source: memory.main\nexpose:\n  app.users: read\n", "legacy perms format"},
		{"empty", "expose: {}\n", "no tables exposed"},
		{"bare name", "expose:\n  users: read\n", "catalog.table"},
		{"too many parts", "expose:\n  a.b.c.d: read\n", "catalog.table"},
		{"bad access", "expose:\n  app.users: write\n", "invalid access"},
		{"reserved catalog", "expose:\n  memory.main.users: read\n", "reserved"},
		{"key not exposed", "expose:\n  app.users: read\nkeys:\n  app.orders: [id]\n", "not exposed"},
		{"empty key", "expose:\n  app.users: readwrite\nkeys:\n  app.users: []\n", "empty"},
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
