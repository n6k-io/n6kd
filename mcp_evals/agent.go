//go:build eval

package mcp_evals

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
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

// Calls counts calls to the MCP tool named tool, on any server.
func (r Result) Calls(tool string) int {
	n := 0
	for _, name := range r.ToolCalls {
		if strings.HasSuffix(name, "__"+tool) {
			n++
		}
	}
	return n
}

func RunAgent(t *testing.T, agent Agent, req Request) Result {
	t.Helper()
	res, err := agent.Run(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tools: %v\nanswer: %s", res.ToolCalls, res.Text)
	return res
}
