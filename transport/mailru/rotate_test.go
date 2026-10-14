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

// rotationServer is a document server that lets every connection in and
// records, per connection in order, the bytes of its Socket.IO events, its
// data cursors and whether it left.
type rotationServer struct {
	*httptest.Server
	mu    sync.Mutex
	conns []*rotationConn
}

type rotationConn struct {
	bytes, cursors int
	left           bool
}

func newRotationServer(t *testing.T) *rotationServer {
	rs := &rotationServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/r7/edit", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"token": "tok",
			"api":   rs.URL,
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
		rc := &rotationConn{}
		rs.mu.Lock()
		rs.conns = append(rs.conns, rc)
		rs.mu.Unlock()
		c.WriteMessage(websocket.TextMessage, []byte(`0{"sid":"eio1","upgrades":[],"pingInterval":25000,"pingTimeout":20000}`))
		for {
			_, m, err := c.ReadMessage()
			if err != nil {
				return
			}
			text := string(m)
			if strings.HasPrefix(text, "40{") {
				c.WriteMessage(websocket.TextMessage, []byte(`40{"sid":"ns1"}`))
			}
			if !strings.HasPrefix(text, "42") {
				continue
			}
			rs.mu.Lock()
			rc.bytes += len(m)
			if strings.Contains(text, `"type":"cursor"`) && !strings.Contains(text, keepAlivePayload) {
				rc.cursors++
			}
			if strings.Contains(text, `"type":"close"`) {
				rc.left = true
			}
			rs.mu.Unlock()
			if strings.Contains(text, `"type":"auth"`) {
				c.WriteMessage(websocket.TextMessage, []byte(`42["message",{"type":"auth","result":1}]`))
			}
		}
	})
	rs.Server = httptest.NewServer(mux)
	return rs
}

func (rs *rotationServer) snapshot() []rotationConn {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([]rotationConn, len(rs.conns))
	for i, c := range rs.conns {
		out[i] = *c
	}
	return out
}

// The document server takes 1 MiB of cursors from a connection and drops
// it after that, so under load a node lost its document every minute or
// so and the tunnel paused while it reconnected. A connection that has
// sent rotateAfter opens its successor, which takes over once it is let
// in; the old one leaves the document, the transport stays connected all
// along and every frame arrives.
func TestConnectionHandsOverBeforeItsQuota(t *testing.T) {
	withAllowance(t, 1000, 1000)
	oldAfter, oldRetire := rotateAfter, retireAfter
	t.Cleanup(func() { rotateAfter, retireAfter = oldAfter, oldRetire })
	rotateAfter, retireAfter = 8<<10, 50*time.Millisecond

	rs := newRotationServer(t)
	defer rs.Close()
	defer func(u string) { editAPIURL = u }(editAPIURL)
	editAPIURL = rs.URL + "/api/v4/r7/edit"

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

	// 30 frames of 1000 bytes, about 1.4 KB a cursor: about 42 KB, five
	// connections' worth.
	const frames = 30
	dropped := false
	for i := 0; i < frames; i++ {
		if err := tr.Send(make([]byte, 1000)); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		for end := time.Now().Add(20 * time.Millisecond); time.Now().Before(end); time.Sleep(time.Millisecond) {
			dropped = dropped || !tr.IsConnected()
		}
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		total := 0
		for _, c := range rs.snapshot() {
			total += c.cursors
		}
		if total == frames {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // the last one replaced leaves

	conns := rs.snapshot()
	total := 0
	for _, c := range conns {
		total += c.cursors
	}
	if total != frames {
		t.Fatalf("%d of %d frames arrived", total, frames)
	}
	if len(conns) < 3 {
		t.Fatalf("%d connections for %d bytes of cursors, want one every %d", len(conns), frames*1400, tr.limits.rotateAfter)
	}
	if dropped {
		t.Error("the transport went disconnected while a connection handed over")
	}
	// Each connection hands over before its quota: it sends at most what
	// it had sent when it started its successor, plus the frames written
	// while the successor came up.
	for i, c := range conns[:len(conns)-1] {
		if !c.left {
			t.Errorf("connection %d was replaced but did not leave the document", i+1)
		}
		if after := int(tr.limits.rotateAfter); c.bytes > 2*after {
			t.Errorf("connection %d sent %d bytes, far past the %d it should hand over at", i+1, c.bytes, after)
		}
	}
	if conns[len(conns)-1].left {
		t.Error("the connection in use left the document")
	}
}
