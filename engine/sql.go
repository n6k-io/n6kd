package engine

import (
	"fmt"
	"strings"
)

// Interpolated because database/sql cannot bind a list; contents come from
// perms.yml, never from a client.
func listLiteral(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = quoteString(item)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
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
