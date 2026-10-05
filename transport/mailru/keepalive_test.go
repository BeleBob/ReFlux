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

// The keep-alive went out every interval whatever the traffic, and the
// document relays it to every editor: both peers' radios woke up every
// 10 s, so a phone's cellular modem never went idle. It must go out only
// when nothing else was written for an interval, and still go out then.
func TestKeepAliveOnlyWhenTheConnectionIsQuiet(t *testing.T) {
	var keepAlives atomic.Int32
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
		c.WriteMessage(websocket.TextMessage, []byte(`40{"sid":"ns1"}`))
		for {
			_, m, err := c.ReadMessage()
			if err != nil {
				return
			}
			if strings.Contains(string(m), "---KA---") {
				keepAlives.Add(1)
			}
		}
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	defer func(u string) { editAPIURL = u }(editAPIURL)
	editAPIURL = srv.URL + "/api/v4/r7/edit"

	cfg := transport.DefaultConfig()
	cfg.KeepAliveInterval = 100 * time.Millisecond
	tr := NewMailruDocsTransport("AbCdEfGh1/IjKlMnOp2", cfg)
	if err := tr.Start(); err != nil {
		t.Fatal(err)
	}
	defer tr.Stop()
	deadline := time.Now().Add(3 * time.Second)
	for !tr.IsConnected() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !tr.IsConnected() {
		t.Fatal("not connected")
	}

	// Traffic every 20 ms for 6 intervals: no keep-alive is needed.
	for end := time.Now().Add(600 * time.Millisecond); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if err := tr.Send([]byte("pkt")); err != nil {
			t.Fatal(err)
		}
	}
	if n := keepAlives.Load(); n != 0 {
		t.Errorf("%d keep-alives while data was flowing", n)
	}

	// Quiet for 6 intervals: keep-alives again.
	time.Sleep(600 * time.Millisecond)
	if n := keepAlives.Load(); n < 3 {
		t.Errorf("%d keep-alives on a quiet connection over 6 intervals, want at least 3", n)
	}
}
