package server

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/duckdb/duckdb-go/v2"
	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/n6k-io/n6kd/engine"
	"github.com/n6k-io/n6kd/internal/wstext"
)

// The extension calls these by name, from its own threads, with the session id
// first. They find the session in hosts.
const (
	authenticateUDF = "n6kd_authenticate"
	onTextUDF       = "n6kd_serve_on_text"
)

// hosts maps a session id to its session for the duration of its CALL. The id
// is random and never sent to the client, so a client cannot name any session.
var hosts sync.Map // duckdb.UUID → *host

type host struct {
	ctx       context.Context
	id        duckdb.UUID
	eng       *engine.Engine
	session   *engine.Session
	mcpServer *mcp.Server
	log       *zap.SugaredLogger

	mu       sync.Mutex
	rejected bool
	admitted bool
	text     *wstext.Conn
	mcp      *mcp.ServerSession
}

func lookupHost(v any) (*host, error) {
	var id duckdb.UUID
	if err := id.Scan(v); err != nil {
		return nil, fmt.Errorf("session id %v: %w", v, err)
	}
	h, ok := hosts.Load(id)
	if !ok {
		return nil, fmt.Errorf("no session %s", id.String())
	}
	return h.(*host), nil
}

// createServeSession hands fd to the extension and returns the id every later
// call into the session names it by.
func createServeSession(ctx context.Context, conn *sql.Conn, fd int) (duckdb.UUID, error) {
	var id duckdb.UUID
	if err := conn.QueryRowContext(ctx, "SELECT session_id FROM n6k_serve_create_session(?)", fd).Scan(&id); err != nil {
		return id, fmt.Errorf("create serve session: %w", err)
	}
	return id, nil
}

// authenticate admits the client into its session and names what it is served.
// The extension sends HELLO_ACK only after this returns, so nothing here may call
// back into the session by id.
func (h *host) authenticate(token string, capabilities []string) (string, error) {
	claims, err := h.eng.Authenticate(h.ctx, token)
	if err != nil {
		h.log.Debugf("token refused: %v", err)
		h.mu.Lock()
		h.rejected = true
		h.mu.Unlock()
		return jsonString(map[string]any{"reject": engine.ErrUnauthorized.Error(), "code": websocket.ClosePolicyViolation})
	}
	if err := h.eng.Admit(h.ctx, h.session, claims, h.log); err != nil {
		return "", err
	}
	appCapabilities := []string{}
	if h.mcpServer != nil && slices.Contains(capabilities, "mcp") {
		if err := h.startMCP(); err != nil {
			return "", err
		}
		appCapabilities = append(appCapabilities, "mcp")
	}
	h.log.Debugf("handing session %s to the reactor", h.id.String())
	h.mu.Lock()
	h.admitted = true
	h.mu.Unlock()
	return jsonString(map[string]any{"catalogs": h.eng.Catalogs(), "app_capabilities": appCapabilities})
}

// The MCP server only answers, so connecting it here sends nothing before
// HELLO_ACK; its replies go out by n6k_serve_send_text, which waits for the
// session to register.
func (h *host) startMCP() error {
	text := wstext.New(textSender{db: h.session.DB(), id: h.id})
	session, err := h.mcpServer.Connect(h.ctx, text, nil)
	if err != nil {
		return fmt.Errorf("start mcp session: %w", err)
	}
	h.mu.Lock()
	h.text, h.mcp = text, session
	h.mu.Unlock()
	return nil
}

func (h *host) onText(text string) {
	h.mu.Lock()
	conn := h.text
	h.mu.Unlock()
	if conn != nil {
		conn.Deliver([]byte(text))
	}
}

func (h *host) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.mcp != nil {
		_ = h.mcp.Close()
	}
	if h.text != nil {
		_ = h.text.Close()
	}
}

// closeSessions ends every admitted WebSocket client of eng with 1001 Going Away.
func closeSessions(ctx context.Context, eng *engine.Engine, log *zap.SugaredLogger) {
	eachAdmitted(eng, func(h *host) {
		if _, err := h.session.DB().ExecContext(ctx, "SELECT * FROM n6k_serve_close(?::UUID, ?::INTEGER, ?)",
			h.id.String(), websocket.CloseGoingAway, "server shutting down"); err != nil {
			log.Warnf("session %s: close: %v", h.id.String(), err)
		}
	})
}

func eachAdmitted(eng *engine.Engine, fn func(*host)) {
	hosts.Range(func(_, v any) bool {
		h := v.(*host)
		h.mu.Lock()
		admitted := h.admitted
		h.mu.Unlock()
		if h.eng == eng && admitted {
			fn(h)
		}
		return true
	})
}

// UpdateServedCatalogs changes what every admitted WebSocket client of eng is
// served; each gets CATALOGS_CHANGED. Added catalogs must already be attached to the
// session's database. A session that fails is logged and left as it was.
func UpdateServedCatalogs(ctx context.Context, eng *engine.Engine, added, removed []string, log *zap.SugaredLogger) {
	eachAdmitted(eng, func(h *host) {
		for _, name := range removed {
			if _, err := h.session.DB().ExecContext(ctx, "SELECT * FROM n6k_serve_remove_catalog(?::UUID, ?)", h.id.String(), name); err != nil {
				log.Warnf("session %s: stop serving %q: %v", h.id.String(), name, err)
			}
		}
		for _, name := range added {
			if _, err := h.session.DB().ExecContext(ctx, "SELECT * FROM n6k_serve_add_catalog(?::UUID, ?)", h.id.String(), name); err != nil {
				log.Warnf("session %s: serve %q: %v", h.id.String(), name, err)
			}
		}
	})
}

type textSender struct {
	db *sql.DB
	id duckdb.UUID
}

func (s textSender) WriteMessage(_ int, data []byte) error {
	_, err := s.db.ExecContext(context.Background(), "SELECT * FROM n6k_serve_send_text(?::UUID, ?)", s.id.String(), string(data))
	return err
}

func jsonString(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// registerCallbacks puts the UDFs on the session's database: once on the shared
// source in direct mode, and on every bridged session's own database.
func registerCallbacks(ctx context.Context, session *engine.Session) error {
	if !session.Shared() {
		return registerCallbackUDFs(ctx, session.DB())
	}
	v, _ := sharedCallbacks.LoadOrStore(session.DB(), &onceErr{})
	once := v.(*onceErr)
	once.Do(func() { once.err = registerCallbackUDFs(ctx, session.DB()) })
	return once.err
}

var sharedCallbacks sync.Map // *sql.DB → *onceErr

type onceErr struct {
	sync.Once
	err error
}

func registerCallbackUDFs(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	uuid, err := duckdb.NewTypeInfo(duckdb.TYPE_UUID)
	if err != nil {
		return err
	}
	varchar, err := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	if err != nil {
		return err
	}
	varcharList, err := duckdb.NewListInfo(varchar)
	if err != nil {
		return err
	}

	authenticate := callbackUDF{
		config: duckdb.ScalarFuncConfig{
			InputTypeInfos:      []duckdb.TypeInfo{uuid, varchar, varcharList, varchar},
			ResultTypeInfo:      varchar,
			Volatile:            true,
			SpecialNullHandling: true,
		},
		run: func(values []driver.Value) (any, error) {
			h, err := lookupHost(values[0])
			if err != nil {
				return nil, err
			}
			token, _ := values[1].(string)
			var capabilities []string
			if list, ok := values[2].([]any); ok {
				for _, c := range list {
					if s, ok := c.(string); ok {
						capabilities = append(capabilities, s)
					}
				}
			}
			return h.authenticate(token, capabilities)
		},
	}
	onText := callbackUDF{
		config: duckdb.ScalarFuncConfig{
			InputTypeInfos:      []duckdb.TypeInfo{uuid, varchar},
			ResultTypeInfo:      varchar,
			Volatile:            true,
			SpecialNullHandling: true,
		},
		run: func(values []driver.Value) (any, error) {
			h, err := lookupHost(values[0])
			if err != nil {
				return nil, err
			}
			text, _ := values[1].(string)
			h.onText(text)
			return nil, nil
		},
	}

	for name, udf := range map[string]callbackUDF{authenticateUDF: authenticate, onTextUDF: onText} {
		if err := duckdb.RegisterScalarUDF(conn, name, udf); err != nil {
			return fmt.Errorf("register %s: %w", name, err)
		}
	}
	return nil
}

type callbackUDF struct {
	config duckdb.ScalarFuncConfig
	run    func([]driver.Value) (any, error)
}

func (u callbackUDF) Config() duckdb.ScalarFuncConfig { return u.config }

func (u callbackUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{RowExecutor: u.run}
}
