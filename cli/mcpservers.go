package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/n6k-io/n6kd/internal/textfmt"
)

func (s *mcpSession) listServers(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.servers) == 0 {
		return textResult("no servers; add one with add_server"), nil, nil
	}
	var lines []string
	for _, name := range slices.Sorted(maps.Keys(s.servers)) {
		sc := s.servers[name]
		state := "disconnected"
		if sc.link != nil && sc.link.alive() {
			state = "connected"
		}
		attached := "none"
		if len(sc.attached) > 0 {
			attached = strings.Join(slices.Sorted(maps.Keys(sc.attached)), ", ")
		}
		lines = append(lines, fmt.Sprintf("%s: %s, %s, %s, attached: %s",
			name, sc.url, state, textfmt.Plural(len(sc.remoteTools), "tool"), attached))
	}
	return textResult(strings.Join(lines, "\n")), nil, nil
}

type addServerInput struct {
	Name    string `json:"name" jsonschema:"name for the server; prefixes its tools as name_tool"`
	URL     string `json:"url" jsonschema:"the server's WebSocket endpoint, as ws://host:port/ws"`
	Persist bool   `json:"persist,omitempty" jsonschema:"also save it to the config file"`
}

func (s *mcpSession) addServer(ctx context.Context, _ *mcp.CallToolRequest, in addServerInput) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := validateServer(in.Name, in.URL); err != nil {
		return nil, nil, err
	}
	if _, taken := s.servers[in.Name]; taken {
		return nil, nil, fmt.Errorf("a server named %q already exists; remove_server it first", in.Name)
	}
	var config mcpConfig
	if in.Persist {
		var err error
		if config, err = loadMCPConfig(s.configPath); err != nil {
			return nil, nil, err
		}
		if _, taken := config.Servers[in.Name]; taken {
			return nil, nil, fmt.Errorf("%s already has a server named %q", s.configPath, in.Name)
		}
	}

	sc := newServerConn(in.Name, in.URL)
	if err := s.connect(ctx, sc); err != nil {
		return nil, nil, errors.Join(err, s.disconnect(context.WithoutCancel(ctx), sc))
	}
	s.servers[in.Name] = sc

	text := fmt.Sprintf("connected %s to %s, serving %s", in.Name, in.URL, textfmt.Plural(len(sc.remoteTools), "tool"))
	if in.Persist {
		config.Servers[in.Name] = mcpServerEntry{URL: in.URL}
		if err := saveMCPConfig(s.configPath, config); err != nil {
			return nil, nil, fmt.Errorf("%s (not saved: %w)", text, err)
		}
		text += ", saved to " + s.configPath
	}
	return textResult(text), nil, nil
}

type removeServerInput struct {
	Name    string `json:"name" jsonschema:"server name from list_servers"`
	Persist bool   `json:"persist,omitempty" jsonschema:"also remove it from the config file"`
}

func (s *mcpSession) removeServer(ctx context.Context, _ *mcp.CallToolRequest, in removeServerInput) (*mcp.CallToolResult, any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sc, err := s.lookup(in.Name)
	if err != nil {
		return nil, nil, err
	}
	var config mcpConfig
	if in.Persist {
		if config, err = loadMCPConfig(s.configPath); err != nil {
			return nil, nil, err
		}
		if _, ok := config.Servers[in.Name]; !ok {
			return nil, nil, fmt.Errorf("%s has no server named %q", s.configPath, in.Name)
		}
	}

	delete(s.servers, in.Name)
	if err := s.disconnect(ctx, sc); err != nil {
		return nil, nil, err
	}
	text := "removed " + in.Name
	if in.Persist {
		delete(config.Servers, in.Name)
		if err := saveMCPConfig(s.configPath, config); err != nil {
			return nil, nil, fmt.Errorf("%s (not saved: %w)", text, err)
		}
		text += " from " + s.configPath
	}
	return textResult(text), nil, nil
}
