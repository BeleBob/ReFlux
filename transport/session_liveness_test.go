package transport

import (
	"sync/atomic"
	"testing"
	"time"
)

// fastKeepalive pings every 20 ms and lets a carrier be silent for 300 ms
// (15 pings) before it is down: with 100 ms a goroutine stalled under
// -race on a busy CI runner made a live carrier look dead.
func fastKeepalive(sessions ...*Session) {
	for _, s := range sessions {
		s.keepaliveInterval = 20 * time.Millisecond
		s.linkTimeout = 300 * time.Millisecond
	}
}

func blackhole(wires ...*startCountingWire) {
	for _, w := range wires {
		w.mu.Lock()
		w.drop = true
		w.mu.Unlock()
	}
}

func sentCount(w *startCountingWire) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.packets)
}

func startPair(t *testing.T, client, exit *Session) {
	t.Helper()
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "both sides ready", func() bool { return client.IsConnected() && exit.IsConnected() })
}

// A carrier can stay attached to its document (IsConnected) while the
// peer's side of it is down; traffic must move to a carrier that still
// reaches the peer.
func TestSessionFailsOverWhenCarrierStopsReachingPeer(t *testing.T) {
	client, exit, cw, ew := linkedSessions(t, "direct", "yandex")
	fastKeepalive(client, exit)
	var got atomic.Int32
	exit.Receive(func([]byte) { got.Add(1) })
	startPair(t, client, exit)
	eventually(t, "keepalive support to be detected", func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.peerKeepalive
	})

	if got := client.ActiveTransport(); got != "direct" {
		t.Fatalf("active transport = %q, want direct", got)
	}
	blackhole(cw["direct"], ew["direct"])
	eventually(t, "data to arrive over the remaining carrier", func() bool {
		_ = client.Send(testIPv4(40, 6))
		return got.Load() > 0
	})
	if !client.IsConnected() {
		t.Fatal("session reported down while a carrier still reaches the peer")
	}
	eventually(t, "the active transport to follow the failover", func() bool {
		return client.ActiveTransport() == "yandex"
	})
}

func TestSessionPrefersHigherPriorityCarrier(t *testing.T) {
	client, exit, cw, _ := linkedSessions(t, "direct", "yandex")
	var got atomic.Int32
	exit.Receive(func([]byte) { got.Add(1) })
	startPair(t, client, exit)

	// The lower-priority carrier stays live (nothing times out in this
	// window) but loses whatever the client sends on it: data that took
	// it would never arrive. Counting its packets instead failed on a busy
	// CI runner, where a late handshake or keepalive packet, which every
	// live carrier carries, fell into the window.
	blackhole(cw["yandex"])
	before := sentCount(cw["direct"])
	for i := 0; i < 5; i++ {
		if err := client.Send(testIPv4(40, 6)); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "data delivery over the preferred carrier", func() bool { return got.Load() == 5 })
	if sentCount(cw["direct"]) == before {
		t.Fatal("nothing went over the preferred carrier")
	}
}

// Peers that predate keepalive never answer pings; their carriers must not
// age out of routing.
func TestSessionToleratesPeerWithoutKeepalive(t *testing.T) {
	client, exit, _, _ := linkedSessions(t, "direct")
	fastKeepalive(client, exit)
	exit.noPong = true
	var got atomic.Int32
	exit.Receive(func([]byte) { got.Add(1) })
	startPair(t, client, exit)

	time.Sleep(3 * client.linkTimeout) // well past when a pinged carrier would be down
	if !client.IsConnected() {
		t.Fatal("carrier aged out against a peer without keepalive")
	}
	if err := client.Send(testIPv4(40, 6)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "data delivery", func() bool { return got.Load() == 1 })
}

// Carriers of equal top priority share the traffic, so the app must be told
// all of them, and not the lower-priority standby.
func TestSessionActiveTransportsNameTheWholeTopGroup(t *testing.T) {
	priorities := []int{100, 100, 50}
	client, exit, _, _ := linkedSessionsWith(t, func(i int) int { return priorities[i] }, "boards", "yandex", "direct")
	fastKeepalive(client, exit)
	startPair(t, client, exit)

	eventually(t, "both top-priority carriers to be active", func() bool {
		got := client.ActiveTransports()
		return len(got) == 2 && got[0] == "boards" && got[1] == "yandex"
	})
	if got := client.ActiveTransport(); got != "boards" {
		t.Fatalf("ActiveTransport = %q, want the first of the group", got)
	}

	_ = client.Stop()
	if got := client.ActiveTransports(); got != nil {
		t.Fatalf("ActiveTransports after Stop = %v, want none", got)
	}
}
