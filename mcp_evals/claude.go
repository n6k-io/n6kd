//go:build eval

package mcp_evals

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// ClaudeCLI runs `claude -p` with only the MCP servers in the request; built-in tools and the user's
// ~/.claude settings and CLAUDE.md are disabled.
type ClaudeCLI struct {
	Model string
}

func (c ClaudeCLI) Run(ctx context.Context, req Request) (Result, error) {
	dir, err := os.MkdirTemp("", "mcp-evals-claude-")
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	args := []string{
		"-p", req.Prompt, "--tools", "", "--setting-sources", "project",
		"--output-format", "stream-json", "--verbose",
	}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	if len(req.MCPServers) > 0 {
		configPath, err := writeClaudeMCPConfig(dir, req.MCPServers)
		if err != nil {
			return Result{}, err
		}
		args = append(args, "--mcp-config", configPath, "--strict-mcp-config")
		for name := range req.MCPServers {
			args = append(args, "--allowedTools", "mcp__"+name)
		}
	}
	if req.Schema != nil {
		args = append(args, "--json-schema", string(req.Schema))
	}

	// #nosec G204 -- args are built here from the eval's own request.
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return Result{}, fmt.Errorf("claude -p: %w\n%s", err, stderr.String())
	}
	return parseStreamJSON(out)
}

func writeClaudeMCPConfig(dir string, servers map[string]MCPServer) (string, error) {
	entries := map[string]any{}
	for name, s := range servers {
		entries[name] = map[string]any{"command": s.Command, "args": s.Args, "env": s.Env}
	}
	raw, err := json.Marshal(map[string]any{"mcpServers": entries})
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "mcp.json")
	return path, os.WriteFile(path, raw, 0o600)
}

func parseStreamJSON(out []byte) (Result, error) {
	var res Result
	sawResult := false
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(nil, 16<<20)
	for scanner.Scan() {
		var event struct {
			Type             string          `json:"type"`
			IsError          bool            `json:"is_error"`
			Result           string          `json:"result"`
			StructuredOutput json.RawMessage `json:"structured_output"`
			Message          struct {
				Content []struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		switch event.Type {
		case "assistant":
			for _, c := range event.Message.Content {
				if c.Type == "tool_use" {
					res.ToolCalls = append(res.ToolCalls, c.Name)
				}
			}
		case "result":
			if event.IsError {
				return res, fmt.Errorf("claude -p: %s", event.Result)
			}
			sawResult = true
			res.Text = event.Result
			res.Structured = event.StructuredOutput
		}
	}
	if err := scanner.Err(); err != nil {
		return res, err
	}
	if !sawResult {
		return res, fmt.Errorf("claude -p: no result event in output")
	}
	return res, nil
}
