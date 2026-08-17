package engine

import (
	"fmt"
	"strings"
)

func primaryKeyMap(keys map[string][]string) map[string]string {
	out := make(map[string]string, len(keys))
	for table, columns := range keys {
		out[table] = strings.Join(columns, ",")
	}
	return out
}

// mapLiteral renders a DuckDB MAP. It is interpolated rather than bound because
// database/sql cannot bind a MAP; the contents come from perms.yml, never from a
// client, and both halves are escaped as string literals regardless.
func mapLiteral(entries map[string]string) string {
	if len(entries) == 0 {
		return "MAP {}::MAP(VARCHAR, VARCHAR)"
	}
	pairs := make([]string, 0, len(entries))
	for key, value := range entries {
		pairs = append(pairs, fmt.Sprintf("%s: %s", quoteString(key), quoteString(value)))
	}
	return "MAP {" + strings.Join(pairs, ", ") + "}"
}

func quoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func qualify(catalog, schema, table string) string {
	return catalog + "." + schema + "." + table
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
