package engine

import (
	"strings"
	"testing"
)

func TestRenderBootTemplate(t *testing.T) {
	t.Setenv("N6KD_TEST_PROJECT", "my-project")
	got, err := renderBootTemplate("boot.sql.template",
		`ATTACH 'billing_project={{ env "N6KD_TEST_PROJECT" }}' AS bq; FROM 'https://x/y.csv?$limit=5';`)
	if err != nil {
		t.Fatal(err)
	}
	want := `ATTACH 'billing_project=my-project' AS bq; FROM 'https://x/y.csv?$limit=5';`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderBootTemplateUnset(t *testing.T) {
	_, err := renderBootTemplate("boot.sql.template", `SELECT '{{ env "N6KD_TEST_UNSET" }}';`)
	if err == nil || !strings.Contains(err.Error(), "N6KD_TEST_UNSET is not set") {
		t.Errorf("got %v, want an unset-variable error", err)
	}
}
