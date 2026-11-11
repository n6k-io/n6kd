package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/n6k-io/n6kd/perms"
	"github.com/n6k-io/n6kd/server"
)

// catalogDocs reads the docs sc serves for catalog, if any, and the URI its
// full docs are served at here.
func (s *mcpSession) catalogDocs(ctx context.Context, sc *serverConn, catalog string) (*perms.Catalog, string, error) {
	jsonURI := server.DocsJSONURI(catalog)
	if _, ok := sc.remoteResources[jsonURI]; !ok || sc.remote == nil {
		return nil, "", nil
	}
	result, err := sc.remote.ReadResource(ctx, &mcp.ReadResourceParams{URI: jsonURI})
	if err != nil {
		return nil, "", fmt.Errorf("read docs of %s: %w", catalog, err)
	}
	if len(result.Contents) == 0 {
		return nil, "", fmt.Errorf("read docs of %s: empty", catalog)
	}
	var docs perms.Catalog
	if err := json.Unmarshal([]byte(result.Contents[0].Text), &docs); err != nil {
		return nil, "", fmt.Errorf("read docs of %s: %w", catalog, err)
	}
	return &docs, sc.remoteResources[server.DocsURI(catalog)], nil
}

// tableDocs finds a table's docs under its perms.yml key, schema.table or
// catalog.schema.table.
func tableDocs(docs *perms.Catalog, catalog, schema, table string) perms.Table {
	if docs == nil {
		return perms.Table{}
	}
	if t, ok := docs.Tables[schema+"."+table]; ok {
		return t
	}
	return docs.Tables[catalog+"."+schema+"."+table]
}
