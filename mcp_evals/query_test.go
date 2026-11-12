//go:build eval

package mcp_evals

import (
	"testing"

	"github.com/n6k-io/n6kd/engine"
)

func TestReadOnlyQueryAnswersWithoutExecute(t *testing.T) {
	RequireExtensions(t)
	agent := ClaudeCLI{}
	orders := StartOrdersServer(t, engine.Options{})

	run := RunAgent(t, agent, Request{
		Prompt:     "Using the n6kd tools, how many orders does alice own on the orders server?",
		MCPServers: map[string]MCPServer{"n6kd": N6kdMCP(t, BuildN6kd(t), map[string]string{"orders": orders})},
	})
	if run.Calls("attach") == 0 {
		t.Error("agent never called attach")
	}
	if run.Calls("query") == 0 {
		t.Error("agent never called query")
	}
	if n := run.Calls("execute"); n != 0 {
		t.Errorf("agent called execute %d times for a read-only question", n)
	}

	verdict := Judge(t, agent, run.Text, map[string]string{
		"states_two": "states that alice owns 2 orders",
	})
	ExpectVerdict(t, verdict, map[string]bool{"states_two": true})
}
