package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"go.uber.org/zap/zaptest"
)

func TestRefusedOriginGets403BeforeAnySession(t *testing.T) {
	srv := httptest.NewServer(newRouter(Config{AllowedOrigins: []string{"https://app.example"}}, nil, zaptest.NewLogger(t).Sugar()))
	t.Cleanup(srv.Close)

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	_, resp, err := websocket.DefaultDialer.Dial(url+"/ws", http.Header{"Origin": {"https://evil.example"}})
	if err == nil {
		t.Fatal("handshake from a refused origin succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("got %v, want 403", resp)
	}
}

func TestCheckOrigin(t *testing.T) {
	for _, test := range []struct {
		name    string
		allowed []string
		origin  string
		want    bool
	}{
		{"wildcard admits any browser", []string{"*"}, "https://evil.example", true},
		{"listed origin admitted", []string{"https://app.example"}, "https://app.example", true},
		{"unlisted origin refused", []string{"https://app.example"}, "https://evil.example", false},
		{"scheme must match", []string{"https://app.example"}, "http://app.example", false},
		{"no Origin header is not a browser", []string{"https://app.example"}, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/ws", nil)
			if test.origin != "" {
				r.Header.Set("Origin", test.origin)
			}
			if got := newOriginCheck(test.allowed)(r); got != test.want {
				t.Errorf("CheckOrigin(%q) with %v = %v, want %v", test.origin, test.allowed, got, test.want)
			}
		})
	}
}
