package transport

import (
	"testing"
	"time"
)

// idlePace scales the idle pacing down for tests: active pings every
// 20 ms (lost after 300 ms), idle after 100 ms without IPv4, then pings
// every 250 ms (lost after 1.5 s).
func idlePace(sessions ...*Session) {
	for _, s := range sessions {
		s.keepaliveInterval = 20 * time.Millisecond
		s.linkTimeout = 300 * time.Millisecond
		s.idleAfter = 100 * time.Millisecond
		s.keepaliveIdle = 250 * time.Millisecond
		s.linkTimeoutIdle = 1500 * time.Millisecond
	}
}

func wireSends(ws ...*startCountingWire) int {
	n := 0
	for _, w := range ws {
		n += sentCount(w)
	}
	return n
}

// Every keepalive is relayed to the peer, which answers: at the active pace
// a phone's radio never went idle while nothing was being sent. With no
// IPv4 the session pings at the idle pace.
func TestSessionPingsLessWhenIdle(t *testing.T) {
	client, exit, cw, ew := linkedSessions(t, "direct")
	idlePace(client, exit)
	startPair(t, client, exit)
	time.Sleep(300 * time.Millisecond) // past idleAfter and the handshake's echoes

	before := wireSends(cw["direct"], ew["direct"])
	time.Sleep(time.Second)
	idle := wireSends(cw["direct"], ew["direct"]) - before
	// At the idle pace, about 4 pings and their pongs; the active pace
	// would be 25 pings and pongs (a ping every other 20 ms tick).
	if idle > 14 {
		t.Errorf("%d frames in 1 s of idle, want at most 14 (pings every 250 ms)", idle)
	}
	if !client.IsConnected() || !exit.IsConnected() {
		t.Fatal("an idle session went down")
	}
}

// When IPv4 resumes after an idle spell, carriers last heard at the idle
// pace may be quieter than the active timeout. They must not be taken for
// lost: no handshake again, no failover off the preferred carrier.
func TestSessionWakesFromIdleWithoutFailover(t *testing.T) {
	client, exit, cw, ew := linkedSessions(t, "direct", "yandex")
	idlePace(client, exit)
	for _, s := range []*Session{client, exit} {
		s.keepaliveIdle = 800 * time.Millisecond // last heard up to ~800 ms ago, past linkTimeout
		s.linkTimeoutIdle = 3 * time.Second
	}
	got := make(chan struct{}, 64)
	exit.Receive(func([]byte) { got <- struct{}{} })
	startPair(t, client, exit)
	// The document relays with a delay: the answers to the pings sent on
	// waking come back after several keepalive ticks.
	for _, w := range []*startCountingWire{cw["direct"], cw["yandex"], ew["direct"], ew["yandex"]} {
		w.mu.Lock()
		w.delay = 100 * time.Millisecond
		w.mu.Unlock()
	}
	eventually(t, "keepalive support to be detected", func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.peerKeepalive
	})
	time.Sleep(time.Second)

	hellos := client.cntHelloSent.Load()
	for i := 0; i < 20; i++ {
		if err := client.Send(testIPv4(40, 6)); err != nil {
			t.Fatalf("send after idle: %v", err)
		}
		if got := client.ActiveTransport(); got != "direct" {
			t.Fatalf("active transport after idle = %q, want direct", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("no data arrived after idle")
	}
	if n := client.cntHelloSent.Load(); n != hellos {
		t.Errorf("handshook again on waking: %d hellos", n-hellos)
	}
}

// A carrier lost while idle is still noticed, at the idle timeout, and
// traffic moves to the one that works.
func TestSessionNoticesALostCarrierWhileIdle(t *testing.T) {
	client, exit, cw, ew := linkedSessions(t, "direct", "yandex")
	idlePace(client, exit)
	got := make(chan struct{}, 64)
	exit.Receive(func([]byte) { got <- struct{}{} })
	startPair(t, client, exit)
	time.Sleep(200 * time.Millisecond)
	blackhole(cw["direct"], ew["direct"])
	time.Sleep(1800 * time.Millisecond) // past linkTimeoutIdle

	client.mu.Lock()
	direct := client.heardLocked(client.links["direct"])
	client.mu.Unlock()
	if direct {
		t.Error("a blackholed carrier still counts as heard after the idle timeout")
	}
	eventually(t, "data over the remaining carrier", func() bool {
		_ = client.Send(testIPv4(40, 6))
		select {
		case <-got:
			return true
		default:
			return false
		}
	})
	if got := client.ActiveTransport(); got != "yandex" {
		t.Errorf("active transport = %q, want yandex", got)
	}
}
