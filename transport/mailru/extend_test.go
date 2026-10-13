package mailru

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// extendServer is a document server that counts extendSession messages and,
// with warn, sends the idle warning once the client has joined.
func extendServer(t *testing.T, warn bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var extends atomic.Int32
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/api/v4/r7/edit", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"token": "tok",
			"api":   srv.URL,
			"document": map[string]any{
				"key": "KEY1", "fileType": "docx", "url": "http://doc", "title": "t.docx",
				"permissions": map[string]any{"edit": true},
			},
			"editorConfig": map[string]any{
				"callbackUrl": "http://cb",
				"user":        map[string]any{"id": "anon1"},
			},
		})
	})
	mux.HandleFunc("/doc/KEY1/c/", func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		c.WriteMessage(websocket.TextMessage, []byte(`0{"sid":"eio1","upgrades":[],"pingInterval":25000,"pingTimeout":20000}`))
		for {
			_, m, err := c.ReadMessage()
			if err != nil {
				return
			}
			msg := string(m)
			if strings.HasPrefix(msg, "40{") {
				c.WriteMessage(websocket.TextMessage, []byte(`40{"sid":"ns1"}`))
			}
			if warn && strings.Contains(msg, `"type":"auth"`) {
				// As Mail.ru sends it (2026-10-13), before the 4002 drop.
				c.WriteMessage(websocket.TextMessage, []byte(`42["message",{"type":"session","messages":{"code":4002,"reason":"idle session expires","interval":3600000}}]`))
			}
			if msg == extendSessionMsg {
				extends.Add(1)
			}
		}
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	old := editAPIURL
	editAPIURL = srv.URL + "/api/v4/r7/edit"
	t.Cleanup(func() { editAPIURL = old })
	return srv, &extends
}

func startExtendTransport(t *testing.T) *MailruDocsTransport {
	t.Helper()
	tr := NewMailruDocsTransport("AbCdEfGh1/IjKlMnOp2", transport.DefaultConfig())
	if err := tr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tr.Stop() })
	return tr
}

func waitExtends(n *atomic.Int32, want int32, within time.Duration) bool {
	for end := time.Now().Add(within); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if n.Load() >= want {
			return true
		}
	}
	return false
}

// Mail.ru closes a session it believes idle for an hour (4002 "idle session
// expires") whatever the tunnel carries: every node dropped its document
// once an hour. The connection renews its session as the editor does.
func TestSessionIsRenewedRegularly(t *testing.T) {
	defer func(d time.Duration) { extendSessionEvery = d }(extendSessionEvery)
	extendSessionEvery = 50 * time.Millisecond
	_, extends := extendServer(t, false)
	startExtendTransport(t)
	if !waitExtends(extends, 3, 3*time.Second) {
		t.Fatalf("%d extendSession messages, want at least 3", extends.Load())
	}
}

// The server warns before it closes an idle session: the answer is a renewal
// at once, not at the next tick.
func TestIdleWarningIsAnsweredAtOnce(t *testing.T) {
	defer func(d time.Duration) { extendSessionEvery = d }(extendSessionEvery)
	extendSessionEvery = time.Hour
	_, extends := extendServer(t, true)
	startExtendTransport(t)
	if !waitExtends(extends, 1, 3*time.Second) {
		t.Fatal("no extendSession after the server's idle warning")
	}
}
