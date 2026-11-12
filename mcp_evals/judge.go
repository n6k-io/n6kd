//go:build eval

package mcp_evals

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
)

// Judge asks a tool-less agent whether answer meets each criterion (name -> description).
// The judge sees only the visible answer, never the transcript.
func Judge(t *testing.T, agent Agent, answer string, criteria map[string]string) map[string]bool {
	t.Helper()
	names := slices.Sorted(maps.Keys(criteria))
	var list strings.Builder
	properties := map[string]any{}
	for _, name := range names {
		list.WriteString("- " + name + ": the message " + criteria[name] + "\n")
		properties[name] = map[string]string{"type": "boolean"}
	}
	schema, err := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             names,
		"additionalProperties": false,
	})
	if err != nil {
		t.Fatal(err)
	}

	prompt := "You are grading an AI assistant's final message to a user. " +
		"Answer each field strictly from what the message says; do not infer intent.\n\n" +
		"Criteria:\n" + list.String() + "\n<message>\n" + answer + "\n</message>"
	res := RunAgent(t, agent, Request{Prompt: prompt, Schema: schema})
	if res.Structured == nil {
		t.Fatalf("judge returned no structured output: %s", res.Text)
	}
	var verdict map[string]bool
	if err := json.Unmarshal(res.Structured, &verdict); err != nil {
		t.Fatalf("judge output %s: %v", res.Structured, err)
	}
	t.Logf("verdict: %v", verdict)
	return verdict
}

func ExpectVerdict(t *testing.T, verdict, want map[string]bool) {
	t.Helper()
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if got, ok := verdict[name]; !ok || got != want[name] {
			t.Errorf("%s = %v, want %v", name, got, want[name])
		}
	}
}
