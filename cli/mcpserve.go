package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	n6kserver "github.com/n6k-io/n6kd/server"
)

func (s *mcpSession) registerServe(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "serve_info",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		Description: "Return the endpoint this process exposes so other clients (e.g. n6kd-ui set_data) " +
			"can query its local DuckDB, as JSON {url, token, catalogs}: every attached alias and memory, " +
			"with the same access as query and execute. It does not list the servers this process is " +
			"connected to; see list_servers.",
	}, s.serveInfo)
}

// serve runs the n6k server over the source until the returned stop is called.
func (s *mcpSession) serve(ctx context.Context, addr string) (stop func() error, err error) {
	serveCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	ready := make(chan string, 1)
	done := make(chan error, 1)
	cfg := n6kserver.Config{Addr: addr, Ready: func(authority string) { ready <- authority }}
	go func() { done <- n6kserver.Run(serveCtx, cfg, s.eng, s.log.Named("serve")) }()
	select {
	case authority := <-ready:
		s.serveURL = "ws://" + authority + "/ws"
	case err := <-done:
		cancel()
		return nil, fmt.Errorf("serve on %s: %w", addr, err)
	}
	return func() error {
		cancel()
		return <-done
	}, nil
}

// syncServed makes the n6k server serve what the source has attached now.
func (s *mcpSession) syncServed(ctx context.Context) {
	added, removed, err := s.eng.RefreshCatalogs(ctx)
	if err != nil {
		s.log.Warnf("refresh served catalogs: %v", err)
		return
	}
	n6kserver.UpdateServedCatalogs(ctx, s.eng, added, removed, s.log)
}

func randomToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

type serveInfo struct {
	URL      string   `json:"url"`
	Token    string   `json:"token"`
	Catalogs []string `json:"catalogs"`
}

func (s *mcpSession) serveInfo(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
	text, err := json.Marshal(serveInfo{URL: s.serveURL, Token: s.serveToken, Catalogs: s.eng.Catalogs()})
	if err != nil {
		return nil, nil, err
	}
	return textResult(string(text)), nil, nil
}
