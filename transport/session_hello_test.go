package transport

import (
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

