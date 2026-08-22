package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/duckdb/duckdb-go/v2"
	"go.uber.org/zap"
)

func varcharToVarchar() duckdb.ScalarFuncConfig {
	varchar, err := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	if err != nil {
		panic(err)
	}
	return duckdb.ScalarFuncConfig{
		InputTypeInfos: []duckdb.TypeInfo{varchar},
		ResultTypeInfo: varchar,
	}
}

// getenvUDF is getenv(name) -> VARCHAR, the same function the duckdb CLI adds,
// NULL when unset. Armed only while a -f file runs: -c has the shell's own
// substitution, and the server's environment holds material (API keys, tokens)
// a served connection must not read. An interval body that needs an env value
// captures it at boot with SET VARIABLE.
type getenvUDF struct {
	mu    sync.Mutex
	armed bool
}

func (u *getenvUDF) setArmed(armed bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.armed = armed
}

func (u *getenvUDF) Config() duckdb.ScalarFuncConfig { return varcharToVarchar() }

func (u *getenvUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			u.mu.Lock()
			armed := u.armed
			u.mu.Unlock()
			if !armed {
				return nil, errors.New("getenv is only available in -f boot SQL")
			}
			name, _ := values[0].(string)
			value, ok := os.LookupEnv(name)
			if !ok {
				return nil, nil
			}
			return value, nil
		},
	}
}

// logUDF is n6kd_log(message) -> VARCHAR, echoing the message into the server
// log. Stays armed so interval bodies can report; in direct mode that means a
// client can write log lines too, which the "sql:" prefix keeps attributable.
type logUDF struct {
	log *zap.SugaredLogger
}

func (u *logUDF) Config() duckdb.ScalarFuncConfig { return varcharToVarchar() }

func (u *logUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			message, _ := values[0].(string)
			u.log.Infof("sql: %s", message)
			return message, nil
		},
	}
}

func registerScalarUDF(ctx context.Context, db *sql.DB, name string, udf duckdb.ScalarFunc) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if err := duckdb.RegisterScalarUDF(conn, name, udf); err != nil {
		return fmt.Errorf("register %s: %w", name, err)
	}
	return nil
}
