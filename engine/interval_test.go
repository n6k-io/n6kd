package engine_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/n6k-io/n6kd/engine"
	"github.com/n6k-io/n6kd/perms"
)

const tickSchemaSQL = schemaSQL + `
SELECT n6kd_set_interval('tick', INTERVAL 1 SECOND,
	'INSERT INTO users SELECT max(id) + 1, ''tick'' FROM users');
`

func openBoot(t *testing.T, boot []engine.Step, rules *perms.File) (*engine.Engine, error) {
	t.Helper()
	if os.Getenv(engine.ExtensionDirEnv) == "" {
		t.Skipf("%s not set", engine.ExtensionDirEnv)
	}
	eng, err := engine.Open(t.Context(), engine.Options{Boot: boot, Perms: rules}, zaptest.NewLogger(t).Sugar())
	if err == nil {
		t.Cleanup(func() {
			if err := eng.Close(); err != nil {
				t.Errorf("close engine: %v", err)
			}
		})
	}
	return eng, err
}

func loadPermsYAML(t *testing.T) *perms.File {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/perms.yml"
	if err := os.WriteFile(path, []byte(permsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err := perms.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func TestSetIntervalRunsAtBootAndTicks(t *testing.T) {
	eng, err := openBoot(t, []engine.Step{{SQL: tickSchemaSQL}}, loadPermsYAML(t))
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	session := newSession(t, eng)

	countUsers := func() int {
		var count int
		if err := session.DB().QueryRowContext(t.Context(),
			`SELECT count(*) FROM app.main.users`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}

	// 2 fixture rows plus the run-at-boot execution of the body.
	if got := countUsers(); got != 3 {
		t.Fatalf("after boot: got %d rows, want 3", got)
	}

	deadline := time.Now().Add(10 * time.Second)
	for countUsers() < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("no tick within 10s: still %d rows", countUsers())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestSetIntervalDuplicateNameFailsBoot(t *testing.T) {
	_, err := openBoot(t, []engine.Step{{SQL: schemaSQL + `
		SELECT n6kd_set_interval('dup', INTERVAL 1 HOUR, 'SELECT 1');
		SELECT n6kd_set_interval('dup', INTERVAL 1 HOUR, 'SELECT 2');
	`}}, loadPermsYAML(t))
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("got %v, want duplicate-name error", err)
	}
}

func TestSetIntervalRejectsMonths(t *testing.T) {
	_, err := openBoot(t, []engine.Step{{SQL: schemaSQL + `
		SELECT n6kd_set_interval('monthly', INTERVAL 1 MONTH, 'SELECT 1');
	`}}, loadPermsYAML(t))
	if err == nil || !strings.Contains(err.Error(), "months") {
		t.Fatalf("got %v, want months error", err)
	}
}

func TestSetIntervalBrokenBodyFailsBoot(t *testing.T) {
	_, err := openBoot(t, []engine.Step{{SQL: schemaSQL + `
		SELECT n6kd_set_interval('broken', INTERVAL 1 HOUR, 'INSERT INTO no_such_table VALUES (1)');
	`}}, loadPermsYAML(t))
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("got %v, want boot failure from broken body", err)
	}
}

// In direct mode the source is served to clients, so a post-boot call must be
// refused rather than scheduling SQL at the source's trust level.
func TestSetIntervalRefusedAfterBoot(t *testing.T) {
	eng, err := openBoot(t, []engine.Step{{SQL: schemaSQL}}, nil)
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	session := newSession(t, eng)

	_, err = session.DB().ExecContext(t.Context(),
		`SELECT n6kd_set_interval('late', INTERVAL 1 SECOND, 'SELECT 1')`)
	if err == nil || !strings.Contains(err.Error(), "only available in boot SQL") {
		t.Fatalf("got %v, want post-boot refusal", err)
	}
}

// The body runs before the call returns, so a later boot statement sees it.
func TestSetIntervalBodyVisibleToLaterBootStatements(t *testing.T) {
	eng, err := openBoot(t, []engine.Step{{SQL: schemaSQL + `
		SELECT n6kd_set_interval('seed', INTERVAL 1 HOUR,
			'INSERT INTO users SELECT max(id) + 1, ''seeded'' FROM users');
		CREATE TABLE seen AS SELECT count(*) AS n FROM users WHERE name = 'seeded';
	`}}, nil)
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	session := newSession(t, eng)

	var seen int
	if err := session.DB().QueryRowContext(t.Context(), `SELECT n FROM memory.main.seen`).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Fatalf("statement after the call saw %d seeded rows, want 1", seen)
	}
}

func TestSetIntervalBodyCannotRegisterAnother(t *testing.T) {
	_, err := openBoot(t, []engine.Step{{SQL: schemaSQL + `
		SELECT n6kd_set_interval('outer', INTERVAL 1 HOUR,
			'SELECT n6kd_set_interval(''inner'', INTERVAL 1 HOUR, ''SELECT 1'')');
	`}}, loadPermsYAML(t))
	if err == nil || !strings.Contains(err.Error(), "while another body runs") {
		t.Fatalf("got %v, want re-entry refusal", err)
	}
}

// The rotating-credential shape: the body re-reads the environment every tick
// and hands the value to a later statement through a variable. SET VARIABLE is
// connection-scoped, so this only works because the body keeps one connection.
func TestSetIntervalBodyRefreshesVariableFromEnv(t *testing.T) {
	t.Setenv("N6KD_TEST_TOKEN", "tok-1")

	eng, err := openBoot(t, []engine.Step{{SQL: schemaSQL + `
		SELECT n6kd_set_interval('refresh', INTERVAL 1 SECOND, '
			SET VARIABLE tok = getenv(''N6KD_TEST_TOKEN'');
			INSERT INTO users SELECT max(id) + 1, getvariable(''tok'') FROM users;
		');
	`}}, loadPermsYAML(t))
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	session := newSession(t, eng)

	seen := func(token string) bool {
		var count int
		if err := session.DB().QueryRowContext(t.Context(),
			`SELECT count(*) FROM app.main.users WHERE name = ?`, token).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count > 0
	}
	if !seen("tok-1") {
		t.Fatal("after boot: body did not write the boot token")
	}

	t.Setenv("N6KD_TEST_TOKEN", "tok-2")
	deadline := time.Now().Add(10 * time.Second)
	for !seen("tok-2") {
		if time.Now().After(deadline) {
			t.Fatal("no tick picked up the rotated token within 10s")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// In direct mode the source is the client's own database, so a body gets no
// getenv window.
func TestSetIntervalBodyGetenvRefusedInDirectMode(t *testing.T) {
	_, err := openBoot(t, []engine.Step{{SQL: schemaSQL + `
		SELECT n6kd_set_interval('env', INTERVAL 1 HOUR, 'SELECT getenv(''HOME'')');
	`}}, nil)
	if err == nil || !strings.Contains(err.Error(), "getenv is only available") {
		t.Fatalf("got %v, want getenv refusal in direct mode", err)
	}
}
