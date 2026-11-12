//go:build eval

package mcp_evals

import (
	"encoding/json"
	"testing"

	"github.com/n6k-io/n6kd/engine"
)

func TestNoLoginTellsUserToLoginAndRestart(t *testing.T) {
	RequireExtensions(t)
	agent := ClaudeCLI{}
	orders := StartServer(t, engine.Options{
		APIKey: "s3cret",
		Boot: BootSQL(t, `
ATTACH ':memory:' AS app;
CREATE TABLE app.main.orders (id INTEGER PRIMARY KEY, owner VARCHAR);
INSERT INTO app.main.orders VALUES (1, 'alice'), (2, 'bob'), (3, 'alice');
`),
	})

	run, err := agent.Run(t.Context(), Request{
		Prompt:     "Using the n6kd tools, how many rows are in the orders table on the orders server?",
		MCPServers: map[string]MCPServer{"n6kd": N6kdMCP(t, BuildN6kd(t), map[string]string{"orders": orders})},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tools: %v\nanswer: %s", run.ToolCalls, run.Text)

	if len(run.ToolCalls) == 0 {
		t.Fatal("agent called no tools")
	}
	if len(run.ToolCalls) > 4 {
		t.Errorf("agent made %d tool calls, want at most 4", len(run.ToolCalls))
	}

	var verdict struct {
		ToldToLogin    bool `json:"told_to_login"`
		ToldToRestart  bool `json:"told_to_restart"`
		ClaimedSuccess bool `json:"claimed_success"`
	}
	err = Judge(t.Context(), agent, `
- told_to_login: the message tells the user to run an "n6kd auth login" command.
- told_to_restart: the message tells the user to restart the n6kd MCP server after logging in.
- claimed_success: the message states a row count as if the query succeeded.`,
		run.Text, json.RawMessage(`{
  "type": "object",
  "properties": {
    "told_to_login": {"type": "boolean"},
    "told_to_restart": {"type": "boolean"},
    "claimed_success": {"type": "boolean"}
  },
  "required": ["told_to_login", "told_to_restart", "claimed_success"],
  "additionalProperties": false
}`), &verdict)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("verdict: %+v", verdict)

	if !verdict.ToldToLogin {
		t.Error("answer does not tell the user to run n6kd auth login")
	}
	if !verdict.ToldToRestart {
		t.Error("answer does not tell the user to restart the n6kd MCP server")
	}
	if verdict.ClaimedSuccess {
		t.Error("answer claims a row count without access")
	}
}
