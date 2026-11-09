package server

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/n6k-io/n6kd/perms"
)

// DocsURIPrefix starts every docs resource URI NewMCP serves.
const DocsURIPrefix = "n6k://docs/"

func DocsURI(catalog string) string { return DocsURIPrefix + url.PathEscape(catalog) }

func TableDocsURI(catalog, table string) string {
	return DocsURI(catalog) + "/" + url.PathEscape(table)
}

// DocsJSONURI serves the catalog's perms.Catalog as JSON.
func DocsJSONURI(catalog string) string { return DocsURI(catalog) + "?format=json" }

// NewMCP serves file's docs as resources and its prompts as <catalog>_<prompt>.
// It returns nil when no catalog has docs.
func NewMCP(file *perms.File, version string) *mcp.Server {
	if file == nil || len(file.Docs) == 0 {
		return nil
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "n6kd", Version: version}, nil)
	for _, name := range slices.Sorted(maps.Keys(file.Docs)) {
		addCatalogDocs(server, name, file.Docs[name])
	}
	return server
}

func addCatalogDocs(server *mcp.Server, name string, docs perms.Catalog) {
	server.AddResource(&mcp.Resource{
		URI: DocsURI(name), Name: name, MIMEType: "text/markdown",
		Description: "Catalog " + name + ": description, hints, and per-table docs",
	}, textResource("text/markdown", catalogMarkdown(name, docs)))

	encoded, err := json.Marshal(docs)
	if err != nil {
		panic(err)
	}
	server.AddResource(&mcp.Resource{
		URI: DocsJSONURI(name), Name: name + ".json", MIMEType: "application/json",
		Description: "Catalog " + name + " docs as JSON",
	}, textResource("application/json", string(encoded)))

	for _, table := range slices.Sorted(maps.Keys(docs.Tables)) {
		var b strings.Builder
		writeTableMarkdown(&b, table, docs.Tables[table])
		server.AddResource(&mcp.Resource{
			URI: TableDocsURI(name, table), Name: name + "." + table, MIMEType: "text/markdown",
			Description: "Table " + table + " of catalog " + name,
		}, textResource("text/markdown", b.String()))
	}

	for _, prompt := range slices.Sorted(maps.Keys(docs.Prompts)) {
		p := docs.Prompts[prompt]
		var args []*mcp.PromptArgument
		for _, arg := range slices.Sorted(maps.Keys(p.Args)) {
			args = append(args, &mcp.PromptArgument{Name: arg, Description: p.Args[arg].Description, Required: p.Args[arg].Required})
		}
		server.AddPrompt(&mcp.Prompt{Name: name + "_" + prompt, Description: p.Description, Arguments: args},
			func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				text, err := p.Render(req.Params.Arguments)
				if err != nil {
					return nil, err
				}
				return &mcp.GetPromptResult{
					Description: p.Description,
					Messages:    []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}},
				}, nil
			})
	}
}

func textResource(mimeType, text string) mcp.ResourceHandler {
	return func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: mimeType, Text: text}}}, nil
	}
}

func catalogMarkdown(name string, docs perms.Catalog) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", name)
	if docs.Description != "" {
		fmt.Fprintf(&b, "\n%s\n", docs.Description)
	}
	if docs.Hints != "" {
		fmt.Fprintf(&b, "\n## Hints\n\n%s\n", strings.TrimRight(docs.Hints, "\n"))
	}
	for _, table := range slices.Sorted(maps.Keys(docs.Tables)) {
		b.WriteString("\n")
		writeTableMarkdown(&b, table, docs.Tables[table])
	}
	return b.String()
}

func writeTableMarkdown(b *strings.Builder, name string, t perms.Table) {
	fmt.Fprintf(b, "## %s\n", name)
	if t.Description != "" {
		fmt.Fprintf(b, "\n%s\n", t.Description)
	}
	if t.Hints != "" {
		fmt.Fprintf(b, "\n%s\n", strings.TrimRight(t.Hints, "\n"))
	}
	if len(t.Columns) > 0 {
		b.WriteString("\nColumns:\n\n")
		for _, column := range slices.Sorted(maps.Keys(t.Columns)) {
			fmt.Fprintf(b, "- %s: %s\n", column, t.Columns[column])
		}
	}
	if len(t.Examples) > 0 {
		b.WriteString("\nExamples:\n\n```sql\n")
		for _, example := range t.Examples {
			b.WriteString(strings.TrimRight(example, "\n") + "\n")
		}
		b.WriteString("```\n")
	}
}
