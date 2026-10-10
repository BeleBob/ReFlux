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

// A connection the network dropped without a word (Wi-Fi to mobile data, a
// NAT forgetting it) stays open on this side: the reader waited on it until
// TCP gave up, minutes later. The server pings every pingInterval, so a
// silence longer than pingInterval + pingTimeout means the connection is
// gone, and the transport reconnects.
func TestSilentConnectionIsReplaced(t *testing.T) {
	var conns atomic.Int32
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
			"editorConfig": map[string]any{"callbackUrl": "http://cb", "user": map[string]any{"id": "anon1"}},
		})
	})
	mux.HandleFunc("/doc/KEY1/c/", func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		conns.Add(1)
		// Pings every 100 ms promised, then none: a dead path.
		c.WriteMessage(websocket.TextMessage, []byte(`0{"sid":"eio1","upgrades":[],"pingInterval":100,"pingTimeout":100}`))
		for {
			_, m, err := c.ReadMessage()
			if err != nil {
				return
			}
			if strings.HasPrefix(string(m), "40{") {
				c.WriteMessage(websocket.TextMessage, []byte(`40{"sid":"ns1"}`))
			}
		}
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	defer func(u string) { editAPIURL = u }(editAPIURL)
	editAPIURL = srv.URL + "/api/v4/r7/edit"
	defer func(d time.Duration) { silenceSlack = d }(silenceSlack)
	silenceSlack = 50 * time.Millisecond

	tr := NewMailruDocsTransport("AbCdEfGh1/IjKlMnOp2", transport.DefaultConfig())
	if err := tr.Start(); err != nil {
		t.Fatal(err)
	}
	defer tr.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for conns.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := conns.Load(); n < 2 {
		t.Fatalf("%d connection(s): a silent connection was kept", n)
	}
}

func TestEngineIOSilence(t *testing.T) {
	for open, want := range map[string]time.Duration{
		`{"sid":"x","pingInterval":25000,"pingTimeout":20000}`: 45*time.Second + silenceSlack,
		`{"sid":"x","pingInterval":1000}`:                      time.Second + silenceSlack,
		`{"sid":"x"}`:                                          defaultSilence,
		`not json`:                                             defaultSilence,
	} {
		if got := engineIOSilence(open); got != want {
			t.Errorf("%s: %v, want %v", open, got, want)
		}
	}
}
