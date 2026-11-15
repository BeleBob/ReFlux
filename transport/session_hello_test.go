package transport

import (
	"crypto/rand"
	"testing"
	"time"
)

// The exit's answer to a client's first hello can be lost (a Telemost lane
// drops most packets in the SFU's first seconds). The client keeps sending
// hellos that do not echo the exit, and the exit must answer those too, or
// the handshake never completes.
func TestSessionExitAnswersAgainWhenItsAnswerWasLost(t *testing.T) {
	client, exit, _, exitWire := sessionPair(t)
	exitWire.mu.Lock()
	exitWire.drop = true
	exitWire.mu.Unlock()

	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- client.Start() }()

	// Let the exit hear the client and answer into the void.
	deadline := time.Now().Add(2 * time.Second)
	for {
		exitWire.mu.Lock()
		sent := len(exitWire.packets)
		exitWire.mu.Unlock()
		if sent > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the exit never answered the client's hello")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	exitWire.mu.Lock()
	exitWire.drop = false
	exitWire.mu.Unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the handshake did not complete after the exit's first answer was lost")
	}
	if _, ready := client.PeerParameters(); !ready {
		t.Fatal("client not ready")
	}
}

// A client can take a challenge the exit has dropped meanwhile: offers that
// waited on a carrier for longer than candidateTTL arrive all at once, the
// client answers the first, and the exit, on a newer challenge or none,
// refuses the answer. The client is ready, the exit is not with it, and
// each drops the other's frames as another session's. A client that has
// just handshaken again does not know yet that its exit answers pings, so
// silence alone does not move it; what it hears from the exit for another
// session must.
func TestSessionClientHandshakesAgainWhenTheExitSpeaksForAnotherSession(t *testing.T) {
	client, exit, _, _ := sessionPair(t)
	fastKeepalive(client, exit)
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}

	exit.mu.Lock()
	if _, err := rand.Read(exit.local[:]); err != nil {
		t.Fatal(err)
	}
	exit.mu.Unlock()
	client.mu.Lock()
	client.peerKeepalive, client.keepaliveProbes = false, keepaliveProbeMax
	client.mu.Unlock()

	eventually(t, "the two sides to share a session again", func() bool {
		client.mu.Lock()
		cl, cp, cr := client.local, client.peer, client.ready
		client.mu.Unlock()
		exit.mu.Lock()
		el, ep, er := exit.local, exit.peer, exit.ready
		exit.mu.Unlock()
		return cr && er && cp == el && ep == cl
	})
}
