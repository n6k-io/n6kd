package sqlquote

import "strings"

func String(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func Ident(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
