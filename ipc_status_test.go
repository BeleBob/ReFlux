package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/ipc"
)

type fixedStats struct{ in, out uint64 }

func (f fixedStats) IsConnected() bool { return true }
func (f fixedStats) Stats() transport.TransportStats {
	return transport.TransportStats{BytesReceived: f.in, BytesSent: f.out}
}

// A classic carrier without a Session reports its traffic too: the app's
// speed counters read it.
func TestIPCStatusWithoutSession(t *testing.T) {
	p := firstStatus(t, nil, nil, nil)
	if !p.Connected || p.BytesIn != 1500 || p.BytesOut != 700 || p.Active != "" || p.HeardMs != 0 {
		t.Fatalf("status = %+v", p)
	}
}

// A Session says when it last heard the peer: an exit's Connected stays
// true while it holds its document, after its client went away.
func TestIPCStatusTellsWhenThePeerWasHeard(t *testing.T) {
	heard := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	if p := firstStatus(t, func() time.Time { return heard }, nil, nil); p.HeardMs != heard.UnixMilli() {
		t.Fatalf("heard_ms = %d, want %d", p.HeardMs, heard.UnixMilli())
	}
	if p := firstStatus(t, func() time.Time { return time.Time{} }, nil, nil); p.HeardMs != 0 {
		t.Fatalf("never heard: heard_ms = %d", p.HeardMs)
	}
}

// A Session says which carriers hold their document: a node with none
// attached cannot be reached, a client there or not.
func TestIPCStatusTellsWhichCarriersAreAttached(t *testing.T) {
	p := firstStatus(t, nil, func() ([]string, int) { return []string{"mailru"}, 2 }, nil)
	if p.Carriers != 2 || len(p.Attached) != 1 || p.Attached[0] != "mailru" {
		t.Fatalf("carriers %d attached %v", p.Carriers, p.Attached)
	}
}

// A Session tells how each carrier is doing: with several documents, the
// host shows which reach the client and the traffic of each.
func TestIPCStatusTellsHowEachCarrierIsDoing(t *testing.T) {
	links := []ipc.LinkStatus{
		{Name: "mailru", Attached: true, Live: true, BytesIn: 10, BytesOut: 20, Flows: 2},
		{Name: "mailru-2", Attached: true},
	}
	p := firstStatus(t, nil, nil, func() []ipc.LinkStatus { return links })
	if len(p.Links) != 2 || p.Links[0] != links[0] || p.Links[1] != links[1] {
		t.Fatalf("links = %+v", p.Links)
	}
}

// firstStatus serves fixedStats with heard, carriers and links over a
// socket and returns the first status an IPC client reads.
func firstStatus(t *testing.T, heard func() time.Time, carriers func() ([]string, int), links func() []ipc.LinkStatus) ipc.StatusPayload {
	t.Helper()
	dir, err := os.MkdirTemp("", "ofipc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "s.sock")
	srv := ipc.NewServer(path, &coreIPCHandler{})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go ipcStatusLoop(srv, fixedStats{in: 1500, out: 700}, nil, nil, heard, carriers, links)

	c, err := ipc.Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got := make(chan ipc.StatusPayload, 1)
	c.SetHandler(func(typ ipc.MsgType, body []byte) {
		if typ != ipc.MsgStatus {
			return
		}
		var p ipc.StatusPayload
		if json.Unmarshal(body, &p) == nil {
			select {
			case got <- p:
			default:
			}
		}
	})
	select {
	case p := <-got:
		return p
	case <-time.After(3 * time.Second):
		t.Fatal("no status")
	}
	return ipc.StatusPayload{}
}
