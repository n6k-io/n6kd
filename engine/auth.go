package engine

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"database/sql/driver"
	"fmt"

	"github.com/duckdb/duckdb-go/v2"
)

// The name n6k_server looks up in duckdb_functions() to decide whether
// handshake auth is on.
const authFunctionName = "n6k_authorize"

// apiKeyAuth is n6k_authorize(token, catalog) -> BOOLEAN over a single shared
// key. Registered as a Go UDF rather than a SQL macro so the key is not stored
// in the catalog.
type apiKeyAuth struct {
	key string
}

func (a *apiKeyAuth) Config() duckdb.ScalarFuncConfig {
	varchar, err := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	if err != nil {
		panic(err)
	}
	boolean, err := duckdb.NewTypeInfo(duckdb.TYPE_BOOLEAN)
	if err != nil {
		panic(err)
	}
	return duckdb.ScalarFuncConfig{
		InputTypeInfos: []duckdb.TypeInfo{varchar, varchar},
		ResultTypeInfo: boolean,
	}
}

func (a *apiKeyAuth) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			token, _ := values[0].(string)
			// An absent token arrives as "": reject it, and never match on an
			// empty key.
			if token == "" || a.key == "" {
				return false, nil
			}
			return subtle.ConstantTimeCompare([]byte(token), []byte(a.key)) == 1, nil
		},
	}
}

// registerAuthorizeUDF registers the verifier on db. RegisterScalarUDF installs
// into the system catalog, so it is visible to every connection on db, not just
// the one used here.
func registerAuthorizeUDF(ctx context.Context, db *sql.DB, key string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if err := duckdb.RegisterScalarUDF(conn, authFunctionName, &apiKeyAuth{key: key}); err != nil {
		return fmt.Errorf("register %s: %w", authFunctionName, err)
	}
	return nil
}
