package mailru

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"openflux/transport"
)

// Mail.ru's document server checks the Socket.IO connect token
// asynchronously and closes the connection without a reason when an event
// arrives before its "40{sid}" answer. The transport sent the auth message
// right behind the token, and every new connection to the document was
// closed within milliseconds: clients could not join, and an exit whose
// connection expired could not rejoin.
func TestAuthWaitsForNamespaceConnect(t *testing.T) {
	frames := make(chan string, 16)
	ack := make(chan struct{})
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
		go func() {
			<-ack
			c.WriteMessage(websocket.TextMessage, []byte(`40{"sid":"ns1"}`))
		}()
		c.WriteMessage(websocket.TextMessage, []byte(`0{"sid":"eio1","upgrades":[],"pingInterval":25000,"pingTimeout":20000}`))
		for {
			_, m, err := c.ReadMessage()
			if err != nil {
				return
			}
			frames <- string(m)
		}
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	defer func(u string) { editAPIURL = u }(editAPIURL)
	editAPIURL = srv.URL + "/api/v4/r7/edit"

	tr := NewMailruDocsTransport("AbCdEfGh1/IjKlMnOp2", transport.DefaultConfig())
	if err := tr.Start(); err != nil {
		t.Fatal(err)
	}
	defer tr.Stop()

	next := func(what string) string {
		t.Helper()
		select {
		case f := <-frames:
			return f
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: nothing arrived", what)
			return ""
		}
	}

	if f := next("connect"); f != `40{"token":"tok"}` {
		t.Fatalf("first frame = %q, want the namespace connect", f)
	}
	select {
	case f := <-frames:
		t.Fatalf("sent %.60q before the server answered the namespace connect", f)
	case <-time.After(200 * time.Millisecond):
	}
	if tr.IsConnected() {
		t.Fatal("connected before the server answered the namespace connect")
	}
	if err := tr.Send([]byte("early")); err == nil {
		t.Fatal("Send accepted a packet before the server answered the namespace connect")
	}

	close(ack)
	if f := next("auth"); !strings.HasPrefix(f, `42["message",{`) || !strings.Contains(f, `"type":"auth"`) {
		t.Fatalf("frame after the answer = %.80q, want the auth message", f)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !tr.IsConnected() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := tr.Send([]byte("pkt")); err != nil {
		t.Fatalf("Send after joining: %v", err)
	}
	for {
		f := next("data")
		if strings.Contains(f, "---KA---") {
			continue
		}
		want := `"cursor":"18;` + base64.StdEncoding.EncodeToString([]byte("pkt")) + `"`
		if !strings.Contains(f, want) {
			t.Fatalf("data frame = %.80q, want %s", f, want)
		}
		break
	}
}
