//go:build eval

package mcp_evals

import "testing"

func TestNoLoginTellsUserToLoginAndRestart(t *testing.T) {
	RequireExtensions(t)
	agent := ClaudeCLI{}
	orders := StartOrdersServer(t)

	run := RunAgent(t, agent, Request{
		Prompt:     "Using the n6kd tools, how many rows are in the orders table on the orders server?",
		MCPServers: map[string]MCPServer{"n6kd": N6kdMCP(t, BuildN6kd(t), map[string]string{"orders": orders})},
	})
	if n := len(run.ToolCalls); n == 0 || n > 4 {
		t.Errorf("agent made %d tool calls, want 1-4", n)
	}

	verdict := Judge(t, agent, run.Text, map[string]string{
		"told_to_login":   `tells the user to run an "n6kd auth login" command`,
		"told_to_restart": "tells the user to restart the n6kd MCP server after logging in",
		"claimed_success": "states a row count as if the query succeeded",
	})
	ExpectVerdict(t, verdict, map[string]bool{
		"told_to_login":   true,
		"told_to_restart": true,
		"claimed_success": false,
	})
}
