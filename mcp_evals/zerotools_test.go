//go:build eval

package mcp_evals

import (
	"testing"

	"github.com/n6k-io/n6kd/engine"
)

func TestAddPlainServerIsNotReportedAsFailure(t *testing.T) {
	RequireExtensions(t)
	agent := ClaudeCLI{}
	shop := StartOrdersServer(t, engine.Options{})

	run := RunAgent(t, agent, Request{
		Prompt:     "Using the n6kd tools, add the n6k server at " + shop + " as shop, then tell me what data it has.",
		MCPServers: map[string]MCPServer{"n6kd": N6kdMCP(t, BuildN6kd(t), nil)},
	})
	if run.Calls("add_server") == 0 {
		t.Error("agent never called add_server")
	}
	if run.Calls("list_catalogs") == 0 {
		t.Error("agent never called list_catalogs")
	}

	verdict := Judge(t, agent, run.Text, map[string]string{
		"reported_problem": "says the server is broken, failed, misconfigured, or has a problem",
		"mentions_orders":  "mentions an orders table",
	})
	ExpectVerdict(t, verdict, map[string]bool{
		"reported_problem": false,
		"mentions_orders":  true,
	})
}
