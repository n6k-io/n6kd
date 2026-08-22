package perms

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	file, err := Parse([]byte(`
source: memory.main
expose:
  app.users: readwrite
  app.audit_log: read
  ops.reporting.events: read
keys:
  app.users: [id]
`))
	if err != nil {
		t.Fatal(err)
	}
	if file.Source.Catalog != "memory" || file.Source.Schema != "main" {
		t.Errorf("source = %+v", file.Source)
	}
	app := file.Catalogs["app"]
	if app.Schema != "main" || app.Tables["users"] != ReadWrite || app.Tables["audit_log"] != Read {
		t.Errorf("app = %+v", app)
	}
	if got := app.PrimaryKeys["users"]; len(got) != 1 || got[0] != "id" {
		t.Errorf("app keys = %v", got)
	}
	if ops := file.Catalogs["ops"]; ops.Schema != "reporting" || ops.Tables["events"] != Read {
		t.Errorf("ops = %+v", ops)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"legacy format", "version: 1\ncatalogs:\n  app:\n    tables:\n      users: read\n", "legacy perms format"},
		{"empty", "expose: {}\n", "no tables exposed"},
		{"bare name", "expose:\n  users: read\n", "catalog.table"},
		{"too many parts", "expose:\n  a.b.c.d: read\n", "catalog.table"},
		{"bad access", "expose:\n  app.users: write\n", "invalid access"},
		{"schema conflict", "expose:\n  app.users: read\n  app.reporting.events: read\n", "two schemas"},
		{"key not exposed", "expose:\n  app.users: read\nkeys:\n  app.orders: [id]\n", "not exposed"},
		{"empty key", "expose:\n  app.users: readwrite\nkeys:\n  app.users: []\n", "empty"},
		{"bad source", "source: a.b.c\nexpose:\n  app.users: read\n", "catalog.schema"},
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
