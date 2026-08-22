package engine_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n6k-io/n6kd/engine"
)

func writeBootFile(t *testing.T, sql string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "boot.sql")
	if err := os.WriteFile(path, []byte(sql), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGetenvInBootFile(t *testing.T) {
	t.Setenv("N6KD_TEST_ENV", "from-the-environment")
	eng, err := openBoot(t, []engine.Step{{Path: writeBootFile(t, schemaSQL+`
		CREATE TABLE env AS SELECT
			getenv('N6KD_TEST_ENV')   AS set_value,
			getenv('N6KD_TEST_UNSET') AS unset_value;
	`)}}, nil)
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	session := newSession(t, eng)

	var value string
	var unsetIsNull bool
	if err := session.DB().QueryRowContext(t.Context(),
		`SELECT set_value, unset_value IS NULL FROM env`).Scan(&value, &unsetIsNull); err != nil {
		t.Fatal(err)
	}
	if value != "from-the-environment" {
		t.Errorf("getenv: got %q, want %q", value, "from-the-environment")
	}
	if !unsetIsNull {
		t.Error("getenv of an unset variable: got a value, want NULL")
	}
}

func TestGetenvRefusedInCommandStep(t *testing.T) {
	_, err := openBoot(t, []engine.Step{{SQL: schemaSQL + `
		SELECT getenv('HOME');
	`}}, nil)
	if err == nil || !strings.Contains(err.Error(), "only available in -f boot SQL") {
		t.Fatalf("got %v, want -c refusal", err)
	}
}

func TestGetenvRefusedAfterBoot(t *testing.T) {
	eng, err := openBoot(t, []engine.Step{{SQL: schemaSQL}}, nil)
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	session := newSession(t, eng)

	var value string
	err = session.DB().QueryRowContext(t.Context(), `SELECT getenv('HOME')`).Scan(&value)
	if err == nil || !strings.Contains(err.Error(), "only available in -f boot SQL") {
		t.Fatalf("got %v, want post-boot refusal", err)
	}
}

func TestLogUDF(t *testing.T) {
	eng, err := openBoot(t, []engine.Step{{SQL: schemaSQL + `
		SELECT n6kd_log('hello from boot');
	`}}, nil)
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	session := newSession(t, eng)

	// Stays armed after boot, so interval bodies can report.
	var echoed string
	if err := session.DB().QueryRowContext(t.Context(),
		`SELECT n6kd_log('hello from a session')`).Scan(&echoed); err != nil {
		t.Fatal(err)
	}
	if echoed != "hello from a session" {
		t.Errorf("n6kd_log echo: got %q", echoed)
	}
}
