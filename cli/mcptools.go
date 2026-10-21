package cli

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/n6k-io/n6kd/internal/textfmt"
)

func (s *mcpSession) register(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "list_servers",
		Description: "List the n6k servers this process holds, one per line: name, url, whether it is " +
			"connected, how many of its MCP tools are served (as name_tool), and the aliases attached from it.",
	}, s.listServers)
	mcp.AddTool(server, &mcp.Tool{
		Name: "add_server",
		Description: "Connect an n6k server under a name and serve its MCP tools as name_tool. With persist, " +
			"also save it to the config file so it is connected at the next start. The token comes from " +
			"the DuckDB secret for the server's host:port (n6kd auth login).",
	}, s.addServer)
	mcp.AddTool(server, &mcp.Tool{
		Name: "remove_server",
		Description: "Disconnect a server, detach its catalogs and drop its tools. With persist, also " +
			"remove it from the config file.",
	}, s.removeServer)
	mcp.AddTool(server, &mcp.Tool{
		Name: "list_catalogs",
		Description: "List the catalogs a server serves, one name per line; with no server, every " +
			"server's as server.catalog. Attach one to query it.",
	}, s.listCatalogs)
	mcp.AddTool(server, &mcp.Tool{
		Name: "attach",
		Description: "Attach a server's catalog to the local DuckDB under an alias. Returns the ATTACH " +
			"statement run and every table the server grants, one per line as " +
			"alias.schema.table: column TYPE, ... Query those tables by that full name.",
	}, s.attach)
	mcp.AddTool(server, &mcp.Tool{
		Name: "query",
		Description: "Run DuckDB SQL that returns rows (SELECT, DESCRIBE, SHOW, SUMMARIZE). Returns CSV " +
			"with a header line, NULL written as NULL, then the row count. Attached tables are " +
			"read on the server, which applies its row policies; everything else runs in a local " +
			"in-memory DuckDB that keeps its state across calls and can read local files.",
	}, s.query)
	mcp.AddTool(server, &mcp.Tool{
		Name: "execute",
		Description: "Run DuckDB SQL for its effect (CREATE, INSERT, UPDATE, DELETE, SET, COPY). Returns " +
			"the statement and the affected row count when known. Writes to attached tables go to " +
			"the server, which refuses what its permissions deny; other objects are local and " +
			"last until this process exits.",
	}, s.execute)
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

type listCatalogsInput struct {
	Server string `json:"server,omitempty" jsonschema:"server name from list_servers; omitted lists every server's"`
}

func (s *mcpSession) listCatalogs(ctx context.Context, _ *mcp.CallToolRequest, in listCatalogsInput) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if in.Server != "" {
		sc, err := s.lookup(in.Server)
		if err != nil {
			return nil, nil, err
		}
		names, err := s.catalogsOf(ctx, sc)
		if err != nil {
			return nil, nil, err
		}
		if len(names) == 0 {
			return textResult(sc.name + " serves no catalogs"), nil, nil
		}
		return textResult(strings.Join(names, "\n")), nil, nil
	}

	var lines []string
	for _, name := range slices.Sorted(maps.Keys(s.servers)) {
		names, err := s.catalogsOf(ctx, s.servers[name])
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		for _, catalog := range names {
			lines = append(lines, name+"."+catalog)
		}
	}
	if len(lines) == 0 {
		return textResult("no server serves a catalog"), nil, nil
	}
	return textResult(strings.Join(lines, "\n")), nil, nil
}

func (s *mcpSession) catalogsOf(ctx context.Context, sc *serverConn) ([]string, error) {
	if err := s.connect(ctx, sc); err != nil {
		return nil, err
	}
	rows, err := s.conn.QueryContext(ctx, "SELECT catalog_name FROM n6k_list_catalogs(wsFd => ?) ORDER BY 1", sc.link.extFD)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

type attachInput struct {
	Server  string `json:"server" jsonschema:"server name from list_servers"`
	Catalog string `json:"catalog" jsonschema:"catalog name from list_catalogs"`
	Alias   string `json:"alias,omitempty" jsonschema:"local name for the catalog; defaults to the catalog name"`
}

func (s *mcpSession) attach(ctx context.Context, _ *mcp.CallToolRequest, in attachInput) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if in.Catalog == "" {
		return nil, nil, errors.New("catalog is required")
	}
	sc, err := s.lookup(in.Server)
	if err != nil {
		return nil, nil, err
	}
	alias := in.Alias
	if alias == "" {
		alias = in.Catalog
	}
	for _, other := range s.servers {
		if _, taken := other.attached[alias]; taken {
			return nil, nil, fmt.Errorf("alias %q is already attached from %s", alias, other.name)
		}
	}
	if err := s.connect(ctx, sc); err != nil {
		return nil, nil, err
	}
	stmt, err := s.attachOverLink(ctx, sc, alias, in.Catalog)
	if err != nil {
		return nil, nil, err
	}
	sc.attached[alias] = in.Catalog

	tables, err := s.describeTables(ctx, alias)
	if err != nil {
		return nil, nil, err
	}
	return textResult(fmt.Sprintf("ran `%s` successfully\n\n%s", stmt, tables)), nil, nil
}

func (s *mcpSession) connectAttached(ctx context.Context) error {
	var errs []error
	for _, sc := range s.servers {
		if len(sc.attached) > 0 {
			if err := s.connect(ctx, sc); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", sc.name, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (s *mcpSession) describeTables(ctx context.Context, alias string) (string, error) {
	rows, err := s.conn.QueryContext(ctx,
		`SELECT schema_name, table_name, column_name, data_type FROM duckdb_columns()
		 WHERE database_name = ? ORDER BY schema_name, table_name, column_index`, alias)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()

	var b strings.Builder
	current := ""
	for rows.Next() {
		var schema, table, column, typ string
		if err := rows.Scan(&schema, &table, &column, &typ); err != nil {
			return "", err
		}
		if name := alias + "." + schema + "." + table; name != current {
			if current != "" {
				b.WriteString("\n")
			}
			current = name
			b.WriteString(name + ": ")
		} else {
			b.WriteString(", ")
		}
		b.WriteString(column + " " + typ)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if current == "" {
		return "no tables", nil
	}
	return b.String(), nil
}

type sqlInput struct {
	SQL string `json:"sql" jsonschema:"DuckDB SQL"`
}

func (s *mcpSession) query(ctx context.Context, _ *mcp.CallToolRequest, in sqlInput) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.connectAttached(ctx); err != nil {
		return nil, nil, err
	}
	rows, err := s.conn.QueryContext(ctx, in.SQL)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()

	columns, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	w := csv.NewWriter(&b)
	if err := w.Write(columns); err != nil {
		return nil, nil, err
	}
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	record := make([]string, len(columns))
	count := 0
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			return nil, nil, err
		}
		for i, v := range values {
			record[i] = csvValue(v)
		}
		if err := w.Write(record); err != nil {
			return nil, nil, err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, nil, err
	}
	return textResult(fmt.Sprintf("%s(%s)", b.String(), textfmt.Plural(count, "row"))), nil, nil
}

func csvValue(v any) string {
	switch v := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}

func (s *mcpSession) execute(ctx context.Context, _ *mcp.CallToolRequest, in sqlInput) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.connectAttached(ctx); err != nil {
		return nil, nil, err
	}
	result, err := s.conn.ExecContext(ctx, in.SQL)
	if err != nil {
		return nil, nil, err
	}
	text := fmt.Sprintf("ran `%s` successfully", in.SQL)
	if n, err := result.RowsAffected(); err == nil {
		text += fmt.Sprintf(", %s affected", textfmt.Plural(int(n), "row"))
	}
	return textResult(text), nil, nil
}
