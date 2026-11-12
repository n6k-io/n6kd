//go:build eval

package mcp_evals

import (
	"context"
	"encoding/json"
)

type MCPServer struct {
	Command string
	Args    []string
	Env     map[string]string
}

type Request struct {
	Prompt     string
	MCPServers map[string]MCPServer
	// Schema, when set, makes Result.Structured hold output that validates against it.
	Schema json.RawMessage
}

type Result struct {
	ToolCalls  []string
	Text       string
	Structured json.RawMessage
}

// Agent runs one prompt to completion. With no MCPServers it has no tools at all.
type Agent interface {
	Run(ctx context.Context, req Request) (Result, error)
}
