//go:build eval

package mcp_evals

import (
	"testing"

	"github.com/n6k-io/n6kd/engine"
)

func TestBadSQLIsCorrectedWithBoundedRetries(t *testing.T) {
	RequireExtensions(t)
	agent := ClaudeCLI{}
	orders := StartOrdersServer(t, engine.Options{})

	run := RunAgent(t, agent, Request{
		Prompt: "Using the n6kd tools, attach the app catalog of the orders server and run " +
			"SELECT count(*) FROM app.main.order WHERE customer = 'alice'. Tell me the count.",
		MCPServers: map[string]MCPServer{"n6kd": N6kdMCP(t, BuildN6kd(t), map[string]string{"orders": orders})},
	})
	if n := run.Calls("query"); n == 0 || n > 3 {
		t.Errorf("agent called query %d times, want 1-3", n)
	}

	verdict := Judge(t, agent, run.Text, map[string]string{
		"states_two": "states that the count of alice's orders is 2",
	})
	ExpectVerdict(t, verdict, map[string]bool{"states_two": true})
}
