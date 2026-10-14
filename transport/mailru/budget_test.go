package mailru

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

func withAllowance(t *testing.T, rate, burst float64) {
	t.Helper()
	oldRate, oldBurst := sendRate, sendBurst
	sendRate, sendBurst = rate, burst
	t.Cleanup(func() { sendRate, sendBurst = oldRate, oldBurst })
}

// Since 2026-10-14 the document server drops a connection that sends more
// than about five messages a second beyond a burst ("access deny"); under
// load the exit sent up to sixty and was dropped every few seconds. Every
// Socket.IO message a connection writes, the auth included, stays within
// its allowance as the server counts it, and every frame still arrives.
func TestConnectionKeepsWithinTheServerAllowance(t *testing.T) {
	const rate, burst = 50.0, 5.0
	withAllowance(t, rate, burst)

	var mu sync.Mutex
	var events []time.Time
	cursors := 0
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
			if strings.HasPrefix(string(m), "40{") {
				c.WriteMessage(websocket.TextMessage, []byte(`40{"sid":"ns1"}`))
			}
			if strings.HasPrefix(string(m), "42") {
				mu.Lock()
				events = append(events, time.Now())
				if strings.Contains(string(m), `"type":"cursor"`) && !strings.Contains(string(m), keepAlivePayload) {
					cursors++
				}
				mu.Unlock()
			}
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
	deadline := time.Now().Add(3 * time.Second)
	for !tr.IsConnected() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !tr.IsConnected() {
		t.Fatal("not connected")
	}

	const frames = 30
	for i := 0; i < frames; i++ {
		if err := tr.Send([]byte{byte(i), 1, 2, 3}); err != nil {
			t.Fatal(err)
		}
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := cursors
		mu.Unlock()
		if n == frames {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if cursors != frames {
		t.Fatalf("%d of %d frames arrived", cursors, frames)
	}
	// A token bucket that starts full: by the time of message i the
	// connection may have sent burst + rate*elapsed messages.
	for i, at := range events {
		allowed := burst + rate*at.Sub(events[0]).Seconds() + 1
		if float64(i+1) > allowed {
			t.Fatalf("message %d went out %v after the first: over the allowance of %.1f",
				i+1, at.Sub(events[0]).Round(time.Millisecond), allowed)
		}
	}
}

// WaitSend lets a batch close only when the connection may send it at
// once: not while the allowance is spent, nor while a frame still waits.
func TestWaitSendWaitsForTheAllowance(t *testing.T) {
	tr := NewMailruDocsTransport("Vuri/test", transport.DefaultConfig())
	s := &DocSession{budget: budget{rate: 20, burst: 2}}
	tr.session = s
	tr.SetConnected(true)

	if !tr.WaitSend(nil) {
		t.Fatal("WaitSend refused a fresh connection")
	}
	s.budget.take()
	s.budget.take()
	start := time.Now()
	if !tr.WaitSend(nil) {
		t.Fatal("WaitSend gave up")
	}
	if waited := time.Since(start); waited < 40*time.Millisecond {
		t.Fatalf("WaitSend returned after %v with the allowance spent, want about 50 ms", waited)
	}

	tr.queued.Add(1)
	stop := make(chan struct{})
	time.AfterFunc(100*time.Millisecond, func() { close(stop) })
	if tr.WaitSend(stop) {
		t.Fatal("WaitSend returned while a frame was still waiting to be written")
	}
}
