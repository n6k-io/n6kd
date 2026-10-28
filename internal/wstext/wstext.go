// Package wstext carries an MCP session on a websocket's text frames, beside the
// n6k binary frames the extension owns. Conn is both the mcp.Transport and its
// Connection; the pumps feed it inbound text with Deliver and write every frame,
// binary included, through WriteMessage so the websocket keeps a single writer.
package wstext

import (
	"context"
	"io"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Conn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
	inbox   chan []byte
	closed  chan struct{}
	once    sync.Once
}

func New(ws *websocket.Conn) *Conn {
	return &Conn{ws: ws, inbox: make(chan []byte, 64), closed: make(chan struct{})}
}

func (c *Conn) WriteMessage(messageType int, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.WriteMessage(messageType, data)
}

// Deliver queues the frame for the session, blocking the calling pump while the
// queue is full, and returns false if the Conn closed first.
func (c *Conn) Deliver(payload []byte) bool {
	select {
	case c.inbox <- payload:
		return true
	case <-c.closed:
		return false
	}
}

func (c *Conn) Connect(context.Context) (mcp.Connection, error) {
	return c, nil
}

func (c *Conn) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case payload := <-c.inbox:
		return jsonrpc.DecodeMessage(payload)
	case <-c.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Conn) Write(_ context.Context, msg jsonrpc.Message) error {
	select {
	case <-c.closed:
		return mcp.ErrConnectionClosed
	default:
	}
	data, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return err
	}
	return c.WriteMessage(websocket.TextMessage, data)
}

// Close ends the MCP session only; the websocket belongs to whoever made the Conn.
func (c *Conn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *Conn) SessionID() string {
	return ""
}
