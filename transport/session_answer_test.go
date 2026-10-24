package transport

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/utils"
)

// oneWayPair starts a client and an exit over a preferred carrier "direct"
// and a standby "yandex". Carriers never time out here (linkTimeout of an
// hour): only the one-way rules (session_answer.go) can take one out. The
// exit echoes what it gets; the returned counter counts the echoes.
func oneWayPair(t *testing.T) (client *Session, cw, ew map[string]*startCountingWire, echoes *atomic.Int32) {
	t.Helper()
	client, exit, cw, ew := linkedSessions(t, "direct", "yandex")
	for _, s := range []*Session{client, exit} {
		s.keepaliveInterval = 50 * time.Millisecond
		s.linkTimeout = time.Hour
		s.idleAfter = 0
		s.peerDataWindow = 300 * time.Millisecond
		s.unansweredAfter = 400 * time.Millisecond
		s.probeUnansweredAfter = 150 * time.Millisecond
	}
	echoes = new(atomic.Int32)
	exit.Receive(func(p []byte) { _ = exit.Send(p) })
	client.Receive(func([]byte) { echoes.Add(1) })
	startPair(t, client, exit)
	for _, s := range []*Session{client, exit} {
		eventually(t, "keepalive support to be detected", func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.peerKeepalive
		})
	}
	return client, cw, ew, echoes
}

// echoed sends packets until n echoes come back, or fails after within.
func echoed(t *testing.T, client *Session, echoes *atomic.Int32, n int32, within time.Duration) time.Duration {
	t.Helper()
	start, want := time.Now(), echoes.Load()+n
	for echoes.Load() < want {
		if time.Since(start) > within {
			t.Fatalf("%d of %d echoes within %v", echoes.Load()-(want-n), n, within)
		}
		_ = client.Send(testIPv4(40, 6))
		time.Sleep(10 * time.Millisecond)
	}
	return time.Since(start)
}

// The exit's packets stop reaching the client on the preferred carrier,
// while the client's still reach the exit there (cups.online on a mobile
// network): the client must give the carrier up and the exit follow it to
// the standby. Both used to stay on it for good, the client because it
// still counted the carrier live, the exit because it still heard the
// client on it.
func TestSessionLeavesACarrierThatDoesNotAnswer(t *testing.T) {
	client, _, ew, echoes := oneWayPair(t)
	echoed(t, client, echoes, 5, 3*time.Second)
	blackhole(ew["direct"])
	took := echoed(t, client, echoes, 20, 5*time.Second)
	t.Logf("echoes again %v after the exit's packets stopped arriving", took.Round(time.Millisecond))
	if got := client.ActiveTransports(); len(got) != 1 || got[0] != "yandex" {
		t.Errorf("client sends on %v, want [yandex]", got)
	}
	comesBack(t, client, echoes, ew["direct"])
}

// comesBack heals w and expects the client back on the preferred carrier.
func comesBack(t *testing.T, client *Session, echoes *atomic.Int32, w *startCountingWire) {
	t.Helper()
	w.mu.Lock()
	w.drop = false
	w.mu.Unlock()
	start := time.Now()
	for {
		echoed(t, client, echoes, 1, 3*time.Second)
		if got := client.ActiveTransports(); len(got) == 1 && got[0] == "direct" {
			break
		}
		if time.Since(start) > 3*time.Second {
			t.Fatalf("the healed carrier was not taken back: client sends on %v", client.ActiveTransports())
		}
	}
	echoed(t, client, echoes, 10, 3*time.Second)
}

// What the exit says about the carriers it answers on: following the
// client's data is an ordinary change of carrier, only a carrier that
// answers nothing is reported as carrying nothing back.
func TestSessionNamesWhyItLeavesACarrier(t *testing.T) {
	var lines []string
	utils.SetLogSink(func(s string) { lines = append(lines, s) })
	t.Cleanup(func() { utils.SetLogSink(nil) })
	said := func(what string) bool {
		for _, l := range lines {
			if strings.Contains(l, what) {
				return true
			}
		}
		return false
	}
	client, _, ew, echoes := oneWayPair(t)
	echoed(t, client, echoes, 5, 3*time.Second)
	if !said(`the client's data comes in on [direct]: answering there`) {
		t.Errorf("the exit did not say where it answers:\n%s", strings.Join(lines, "\n"))
	}
	if said("carry nothing back") {
		t.Errorf("a working carrier was called broken:\n%s", strings.Join(lines, "\n"))
	}
	lines = nil
	blackhole(ew["direct"])
	echoed(t, client, echoes, 20, 5*time.Second)
	if !said(`[direct] carry nothing back from the peer`) {
		t.Errorf("the dead carrier was not named:\n%s", strings.Join(lines, "\n"))
	}
}

// The other way round: the client's packets stop reaching the exit on the
// preferred carrier while the exit's still arrive.
func TestSessionLeavesACarrierThePeerDoesNotHear(t *testing.T) {
	client, cw, _, echoes := oneWayPair(t)
	echoed(t, client, echoes, 5, 3*time.Second)
	blackhole(cw["direct"])
	took := echoed(t, client, echoes, 20, 5*time.Second)
	t.Logf("echoes again %v after the client's packets stopped arriving", took.Round(time.Millisecond))
	comesBack(t, client, echoes, cw["direct"])
}
