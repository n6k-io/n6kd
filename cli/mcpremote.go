package cli

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"
	"time"

	"github.com/duckdb/duckdb-go/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/n6k-io/n6kd/internal/wstext"
)

// Skips server/discover, which servers on the text-frame transport are not known to answer.
const remoteProtocolVersion = "2025-11-25"

const (
	onTextUDF  = "n6kd_on_text"
	onCloseUDF = "n6kd_on_close"

	onCloseWait = time.Second
)

type textRoute struct {
	conn   *wstext.Conn
	closed chan struct{}
}

// textRoutes maps connection ids to their MCP sessions. The UDFs run on the
// extension's threads, so nothing here may wait on mcpSession.mu.
type textRoutes struct {
	mu     sync.Mutex
	routes map[string]*textRoute
}

func newTextRoutes() *textRoutes {
	return &textRoutes{routes: map[string]*textRoute{}}
}

func (r *textRoutes) register(conn *sql.Conn) error {
	if err := duckdb.RegisterScalarUDF(conn, onTextUDF, connectionUDF(r.deliver)); err != nil {
		return fmt.Errorf("register %s: %w", onTextUDF, err)
	}
	onClose := func(id, _ string) { r.close(id) }
	if err := duckdb.RegisterScalarUDF(conn, onCloseUDF, connectionUDF(onClose)); err != nil {
		return fmt.Errorf("register %s: %w", onCloseUDF, err)
	}
	return nil
}

func (r *textRoutes) open(id string, writer wstext.Writer) *wstext.Conn {
	conn := wstext.New(writer)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes[id] = &textRoute{conn: conn, closed: make(chan struct{})}
	return conn
}

func (r *textRoutes) has(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.routes[id]
	return ok
}

func (r *textRoutes) deliver(id, text string) {
	r.mu.Lock()
	route, ok := r.routes[id]
	r.mu.Unlock()
	if ok {
		route.conn.Deliver([]byte(text))
	}
}

func (r *textRoutes) close(id string) {
	r.mu.Lock()
	route, ok := r.routes[id]
	delete(r.routes, id)
	r.mu.Unlock()
	if ok {
		_ = route.conn.Close()
		close(route.closed)
	}
}

func (r *textRoutes) awaitClose(ctx context.Context, id string) {
	r.mu.Lock()
	route, ok := r.routes[id]
	r.mu.Unlock()
	if !ok {
		return
	}
	select {
	case <-route.closed:
	case <-time.After(onCloseWait):
		r.close(id)
	case <-ctx.Done():
		r.close(id)
	}
}

// connectionUDF is the (connection_id UUID, text VARCHAR) shape the extension calls.
type connectionUDF func(id, text string)

func (f connectionUDF) Config() duckdb.ScalarFuncConfig {
	id, err := duckdb.NewTypeInfo(duckdb.TYPE_UUID)
	if err != nil {
		panic(err)
	}
	text, err := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	if err != nil {
		panic(err)
	}
	return duckdb.ScalarFuncConfig{InputTypeInfos: []duckdb.TypeInfo{id, text}, ResultTypeInfo: text, Volatile: true}
}

func (f connectionUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{RowExecutor: func(values []driver.Value) (any, error) {
		var id duckdb.UUID
		if err := id.Scan(values[0]); err != nil {
			return nil, err
		}
		text, ok := values[1].(string)
		if !ok {
			return nil, fmt.Errorf("text argument is %T", values[1])
		}
		f(id.String(), text)
		return nil, nil
	}}
}

type textSender struct {
	conn *sql.Conn
	id   string
}

func (w textSender) WriteMessage(_ int, data []byte) error {
	_, err := w.conn.ExecContext(context.Background(), "SELECT * FROM n6k_send_text(?, ?)", w.id, string(data))
	return err
}

func (s *mcpSession) connectRemote(ctx context.Context, sc *serverConn) error {
	client := mcp.NewClient(&mcp.Implementation{Name: "n6kd", Version: buildVersion()}, nil)
	text := s.texts.open(sc.id, textSender{conn: s.sendConn, id: sc.id})
	remote, err := client.Connect(ctx, text, &mcp.ClientSessionOptions{ProtocolVersion: remoteProtocolVersion})
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
