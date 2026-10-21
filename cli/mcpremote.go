package cli

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Skips server/discover, which servers on the text-frame transport are not known to answer.
const remoteProtocolVersion = "2025-11-25"

func (s *mcpSession) connectRemote(ctx context.Context, sc *serverConn) error {
	client := mcp.NewClient(&mcp.Implementation{Name: "n6kd", Version: buildVersion()}, nil)
	remote, err := client.Connect(ctx, sc.link.text, &mcp.ClientSessionOptions{ProtocolVersion: remoteProtocolVersion})
	if err != nil {
		return fmt.Errorf("mcp session with %s: %w", sc.url, err)
	}
	sc.remote = remote

	for tool, err := range remote.Tools(ctx, nil) {
		if err != nil {
			return fmt.Errorf("list tools of %s: %w", sc.url, err)
		}
		if schema, ok := tool.InputSchema.(map[string]any); !ok || schema["type"] != "object" {
			s.log.Warnf("%s: skipping remote tool %q: its input schema is not an object", sc.name, tool.Name)
			continue
		}
		local := *tool
		local.Name = sc.name + "_" + tool.Name
		s.server.AddTool(&local, s.forward(sc, tool.Name))
		sc.remoteTools = append(sc.remoteTools, local.Name)
	}
	s.log.Infof("%s: serving %d tools of %s as %s_*", sc.name, len(sc.remoteTools), sc.url, sc.name)
	return nil
}

func (s *mcpSession) disconnectRemote(sc *serverConn) error {
	s.server.RemoveTools(sc.remoteTools...)
	sc.remoteTools = nil
	if sc.remote == nil {
		return nil
	}
	err := sc.remote.Close()
	sc.remote = nil
	return err
}

func (s *mcpSession) forward(sc *serverConn, name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		s.mu.Lock()
		var err error
		if s.servers[sc.name] != sc {
			err = fmt.Errorf("server %q was removed", sc.name)
		} else {
			err = s.connect(ctx, sc)
		}
		remote := sc.remote
		s.mu.Unlock()
		if err != nil {
			return errorResult(err), nil
		}
		if remote == nil {
			return errorResult(fmt.Errorf("%s no longer serves %s", sc.url, name)), nil
		}
		params := &mcp.CallToolParams{Name: name}
		if len(req.Params.Arguments) > 0 {
			params.Arguments = req.Params.Arguments
		}
		return remote.CallTool(ctx, params)
	}
}

func errorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}
