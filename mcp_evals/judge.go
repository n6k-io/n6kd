//go:build eval

package mcp_evals

import (
	"context"
	"encoding/json"
	"fmt"
)

// Judge asks a tool-less agent to grade answer against criteria and decodes its schema-validated verdict into out.
// The judge sees only the visible answer, never the transcript.
func Judge(ctx context.Context, agent Agent, criteria, answer string, schema json.RawMessage, out any) error {
	prompt := "You are grading an AI assistant's final message to a user. " +
		"Answer each field strictly from what the message says; do not infer intent.\n\n" +
		"Criteria:\n" + criteria + "\n\n<message>\n" + answer + "\n</message>"
	res, err := agent.Run(ctx, Request{Prompt: prompt, Schema: schema})
	if err != nil {
		return fmt.Errorf("judge: %w", err)
	}
	if res.Structured == nil {
		return fmt.Errorf("judge returned no structured output: %s", res.Text)
	}
	if err := json.Unmarshal(res.Structured, out); err != nil {
		return fmt.Errorf("judge output %s: %w", res.Structured, err)
	}
	return nil
}
