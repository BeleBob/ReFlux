package yandex

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// fakeTelemost is the conference API and the media server's WebSocket: every
// connection request makes a new participant, and a participant whose
// WebSocket has closed is refused when it says hello again, the way the SFU
// does (close 4000 "TIE-0003").
type fakeTelemost struct {
	api, ws *httptest.Server

	mu     sync.Mutex
	joined int
	hellos []string        // participant of every hello, in order
	closed map[string]bool // participants whose WebSocket ended
	conns  map[string]*websocket.Conn
}

func newFakeTelemost(t *testing.T) *fakeTelemost {
	f := &fakeTelemost{closed: map[string]bool{}, conns: map[string]*websocket.Conn{}}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	f.ws = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, b, err := c.ReadMessage()
		if err != nil {
			return
		}
		var m struct {
			Hello struct {
				ParticipantID string `json:"participantId"`
			} `json:"hello"`
		}
		_ = json.Unmarshal(b, &m)
		p := m.Hello.ParticipantID
		f.mu.Lock()
		f.hellos = append(f.hellos, p)
		refused := f.closed[p]
		f.conns[p] = c
		f.mu.Unlock()
		if refused {
			_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4000, "TIE-0003"))
			return
		}
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				break
			}
		}
		f.mu.Lock()
		f.closed[p] = true
		f.mu.Unlock()
	}))
	f.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/connection") {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		f.joined++
		p := fmt.Sprintf("participant-%d", f.joined)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"room_id": "room", "peer_id": p, "credentials": "c-" + p,
			"client_configuration": map[string]interface{}{"media_server_url": "ws" + strings.TrimPrefix(f.ws.URL, "http")},
		})
	}))
	saved := telemostAPI
	telemostAPI = f.api.URL
	t.Cleanup(func() {
		telemostAPI = saved
		f.api.Close()
		f.ws.CloseClientConnections()
		f.ws.Close()
	})
	return f
}

// drop ends participant p's WebSocket from the server side.
func (f *fakeTelemost) drop(p string) {
	f.mu.Lock()
	c := f.conns[p]
	f.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

func (f *fakeTelemost) helloList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hellos...)
}

// A lane whose WebSocket drops comes back as a new participant in its own
// place: redialling as the old one is refused for good.
func TestTelemostLaneRejoinsAsANewParticipant(t *testing.T) {
	f := newFakeTelemost(t)
	g := NewTelemostGroup("", "https://telemost.yandex.ru/j/1", 1, true, transport.DefaultConfig())
	type swapped struct {
		idx int
		l   transport.Transport
	}
	swaps := make(chan swapped, 4)
	g.SetSwapHook(func(idx int, l transport.Transport) { swaps <- swapped{idx, l} })
	lane := g.Lanes()[0].(*TelemostTransport)
	if err := lane.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lane.Stop() })

	deadline := time.Now().Add(3 * time.Second)
	for len(f.helloList()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the lane never said hello")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.drop("participant-1")

	select {
	case s := <-swaps:
		t.Cleanup(func() { _ = s.l.Stop() })
		nl := s.l.(*TelemostTransport)
		if s.idx != 0 || nl.LaneID() != "participant-2" || g.Lanes()[0] != s.l {
			t.Fatalf("lane %d replaced by %q", s.idx, nl.LaneID())
		}
		if nl.laneName != lane.laneName {
			t.Error("the new participant does not carry the lane's tag")
		}
	case <-time.After(6 * time.Second):
		t.Fatalf("no new participant took the lane's place; hellos: %v", f.helloList())
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		h := f.helloList()
		if h[len(h)-1] == "participant-2" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the new participant never said hello: %v", h)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
