package transport

import (
	"testing"
	"time"
)

// An exit whose client went away still reads connected while its own
// carrier holds the document (IsConnected counts a carrier attached on
// this side), so a status that means "the client is there" reads
// PeerHeard: it follows the client's pings, and stops when they do.
func TestSessionPeerHeardStopsWhenTheClientGoes(t *testing.T) {
	client, exit, cw, ew := linkedSessions(t, "direct")
	fastKeepalive(client, exit)
	if !exit.PeerHeard().IsZero() {
		t.Fatal("heard before the handshake")
	}
	startPair(t, client, exit)
	eventually(t, "the client's pings to be heard", func() bool {
		return time.Since(exit.PeerHeard()) < exit.linkTimeout/2
	})

	blackhole(cw["direct"], ew["direct"])
	time.Sleep(3 * exit.linkTimeout)
	heard := exit.PeerHeard()
	if heard.IsZero() || time.Since(heard) < 2*exit.linkTimeout {
		t.Fatalf("heard %v ago: the client is gone", time.Since(heard))
	}
	if !exit.IsConnected() {
		t.Log("IsConnected no longer counts a carrier attached only on this side")
	}
	// A carrier marked stalled forgets the peer for routing, not for this.
	exit.MarkStalled("direct")
	if !exit.PeerHeard().Equal(heard) {
		t.Error("MarkStalled cleared PeerHeard")
	}
}

// Carriers tells which carriers hold their document, a peer there or not:
// none before Start, every one once started, and not one marked dead.
func TestSessionCarriersSayWhatIsAttached(t *testing.T) {
	client, exit, _, _ := linkedSessions(t, "direct", "yandex")
	if attached, total := exit.Carriers(); len(attached) != 0 || total != 2 {
		t.Fatalf("before Start: %v of %d", attached, total)
	}
	startPair(t, client, exit)
	if attached, total := exit.Carriers(); len(attached) != 2 || total != 2 {
		t.Fatalf("started: %v of %d", attached, total)
	}
	exit.MarkDead("yandex")
	if attached, total := exit.Carriers(); len(attached) != 1 || attached[0] != "direct" || total != 2 {
		t.Fatalf("yandex dead: %v of %d", attached, total)
	}
}
