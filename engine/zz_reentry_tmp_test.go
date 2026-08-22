package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"testing"
	"time"

	"github.com/duckdb/duckdb-go/v2"
)

type reentrantUDF struct {
	db *sql.DB
	t  *testing.T
}

func (u *reentrantUDF) Config() duckdb.ScalarFuncConfig { return varcharToVarchar() }

func (u *reentrantUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			body, _ := values[0].(string)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := u.db.ExecContext(ctx, body); err != nil {
				u.t.Logf("inline exec failed: %v", err)
				return nil, err
			}
			return body, nil
		},
	}
}

func TestReentrantInlineExec(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("duckdb", extensionDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if err := registerScalarUDF(ctx, db, "inline_exec", &reentrantUDF{db: db, t: t}); err != nil {
		t.Fatal(err)
	}

	// Same connection pool, statement-in-statement.
	if _, err := db.ExecContext(ctx, `CREATE TABLE t (id INTEGER, v VARCHAR)`); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := db.ExecContext(ctx, `
			SELECT inline_exec('INSERT INTO t VALUES (1, ''from-udf'')');
			CREATE TABLE seen AS SELECT count(*) AS n FROM t;
		`)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("boot exec: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("DEADLOCK: nested exec never returned")
	}

	var n int
	if err := db.QueryRowContext(ctx, `SELECT n FROM seen`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	t.Logf("next statement in the same Exec saw %d row(s)", n)
	if n != 1 {
		t.Errorf("want 1 row visible to the following statement, got %d", n)
	}
}

// Same table written by both the outer statement and the inline body.
func TestReentrantInlineExecWriteConflict(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("duckdb", extensionDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if err := registerScalarUDF(ctx, db, "inline_exec", &reentrantUDF{db: db, t: t}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE t (id INTEGER, v VARCHAR); INSERT INTO t VALUES (0, 'seed')`); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := db.ExecContext(ctx,
			`INSERT INTO t SELECT 2, inline_exec('INSERT INTO t VALUES (1, ''from-udf'')')`)
		done <- err
	}()
	select {
	case err := <-done:
		t.Logf("outer write + inline write into same table: err=%v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("DEADLOCK: nested write never returned")
	}
}

// Many rows -> the UDF runs on multiple scheduler threads at once.
func TestReentrantInlineExecParallel(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("duckdb", extensionDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	if err := registerScalarUDF(ctx, db, "inline_exec", &reentrantUDF{db: db, t: t}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE t (id INTEGER, v VARCHAR)`); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := db.ExecContext(ctx,
			`SELECT inline_exec('INSERT INTO t VALUES (1, ''x'')') FROM range(2000)`)
		done <- err
	}()
	select {
	case err := <-done:
		t.Logf("2000 concurrent inline execs: err=%v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("DEADLOCK: parallel nested exec never returned")
	}
}
