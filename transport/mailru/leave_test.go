package mailru

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"openflux/transport"
)

// A stopped transport must leave the document the way the editor does:
// Mail.ru keeps a participant that just drops its socket listed for
// minutes and turns every new joiner away meanwhile, so a restarted exit
// could not rejoin while its client stayed in the document.
func TestStopLeavesTheDocument(t *testing.T) {
	type frame struct {
		text string
		code int // close code, for the close frame
	}
	frames := make(chan frame, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		for {
			_, m, err := c.ReadMessage()
			if err != nil {
				if ce, ok := err.(*websocket.CloseError); ok {
					frames <- frame{code: ce.Code}
				}
				close(frames)
				return
			}
			frames <- frame{text: string(m)}
		}
	}))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := NewMailruDocsTransport("AbCdEfGh1/IjKlMnOp2", transport.DefaultConfig())
	if err := tr.BaseTransport.Start(); err != nil {
		t.Fatal(err)
	}
	tr.session = &DocSession{Conn: conn, WriteQueue: make(chan []byte, 1)}
	if err := tr.Stop(); err != nil {
		t.Fatal(err)
	}

	want := []frame{
		{text: `42["message",{"type":"close"}]`},
		{text: "41"},
		{code: websocket.CloseNormalClosure},
	}
	for i, w := range want {
		select {
		case got, ok := <-frames:
			if !ok {
				t.Fatalf("frame %d: connection ended early", i)
			}
			if got != w {
				t.Errorf("frame %d = %+v, want %+v", i, got, w)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("frame %d: nothing arrived", i)
		}
	}
	if tr.IsRunning() {
		t.Error("transport still running after Stop")
	}
}

// Stop without a connection (never connected, or between reconnects)
// must not fail or block.
func TestStopWithoutASession(t *testing.T) {
	tr := NewMailruDocsTransport("AbCdEfGh1/IjKlMnOp2", transport.DefaultConfig())
	if err := tr.BaseTransport.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- tr.Stop() }()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop blocked")
	}
}
