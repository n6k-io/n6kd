package cli

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/n6k-io/n6kd/perms"
)

func (s *mcpSession) describeTables(ctx context.Context, alias, catalog string, docs *perms.Catalog) (string, error) {
	rows, err := s.conn.QueryContext(ctx,
		`SELECT c.schema_name, c.table_name, c.column_name, c.data_type, coalesce(t.comment, ''), coalesce(c.comment, '')
		 FROM duckdb_columns() c LEFT JOIN duckdb_tables() t USING (database_name, schema_name, table_name)
		 WHERE c.database_name = ? ORDER BY c.schema_name, c.table_name, c.column_index`, alias)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()

	var b strings.Builder
	current := ""
	var notes perms.Table
	flush := func() {
		if notes.Description != "" {
			b.WriteString("\n  " + notes.Description)
		}
		for _, column := range slices.Sorted(maps.Keys(notes.Columns)) {
			b.WriteString("\n  " + column + ": " + notes.Columns[column])
		}
	}
	for rows.Next() {
		var schema, table, column, typ, tableComment, columnComment string
		if err := rows.Scan(&schema, &table, &column, &typ, &tableComment, &columnComment); err != nil {
			return "", err
		}
		if name := alias + "." + schema + "." + table; name != current {
			if current != "" {
				flush()
				b.WriteString("\n")
			}
			current = name
			notes = tableDocs(docs, catalog, schema, table)
			notes.Columns = maps.Clone(notes.Columns)
			if notes.Description == "" {
				notes.Description = tableComment
			}
			b.WriteString(name + ": ")
		} else {
			b.WriteString(", ")
		}
		b.WriteString(column + " " + typ)
		if _, ok := notes.Columns[column]; !ok && columnComment != "" {
			if notes.Columns == nil {
				notes.Columns = map[string]string{}
			}
			notes.Columns[column] = columnComment
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if current == "" {
		return "no tables", nil
	}
	flush()
	return "tables:\n" + b.String(), nil
}

func (s *mcpSession) describeFunctions(ctx context.Context, alias string) (string, error) {
	rows, err := s.conn.QueryContext(ctx,
		`SELECT schema_name, function_name, function_type,
		        array_to_string(list_transform(list_zip(parameters, parameter_types), lambda p: concat_ws(' ', p[1], p[2])), ', '),
		        coalesce(comment, '')
		 FROM duckdb_functions() WHERE database_name = ? ORDER BY schema_name, function_name`, alias)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()

	var lines []string
	for rows.Next() {
		var schema, name, kind, args, comment string
		if err := rows.Scan(&schema, &name, &kind, &args, &comment); err != nil {
			return "", err
		}
		line := fmt.Sprintf("%s.%s.%s(%s): %s", alias, schema, name, args, kind)
		if comment != "" {
			line += "\n  " + comment
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), rows.Err()
}
