package server

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type whoami struct {
	Auth   string         `json:"auth"`
	Claims map[string]any `json:"claims,omitempty"`
}

func addWhoami(server *mcp.Server, auth AuthMode, claims map[string]any) {
	encoded, err := json.Marshal(whoami{Auth: auth.String(), Claims: claims})
	if err != nil {
		panic(err)
	}
	mcp.AddTool(server, &mcp.Tool{
		Name: "whoami",
		Description: "How this connection authenticated (none, api_key, or oidc) and, for oidc, the " +
			"scalar JWT claims perms.yml sees as $name.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}}, nil, nil
	})
}
