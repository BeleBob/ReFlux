package transport

import (
	"bytes"
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

type negotiationWire struct {
	mu      sync.Mutex
	cb      func([]byte)
	peer    *negotiationWire
	packets [][]byte
	drop    bool
}

func (w *negotiationWire) Start() error            { return nil }
func (w *negotiationWire) Stop() error             { return nil }
func (w *negotiationWire) IsConnected() bool       { return true }
func (w *negotiationWire) Stats() TransportStats   { return TransportStats{} }
func (w *negotiationWire) Receive(cb func([]byte)) { w.mu.Lock(); w.cb = cb; w.mu.Unlock() }
func (w *negotiationWire) Send(p []byte) error {
	p = append([]byte(nil), p...)
	w.mu.Lock()
	w.packets = append(w.packets, p)
	drop := w.drop
	w.mu.Unlock()
	if w.peer != nil && !drop {
		w.peer.mu.Lock()
		cb := w.peer.cb
		w.peer.mu.Unlock()
		if cb != nil {
			cb(p)
		}
	}
	return nil
}

func negotiatedPair(t *testing.T, batched bool) (*NegotiatedTransport, *NegotiatedTransport, *negotiationWire, *negotiationWire) {
	t.Helper()
	a, b := &negotiationWire{}, &negotiationWire{}
	a.peer = b
	b.peer = a
	var aw, bw Transport = a, b
	if batched {
		aw = NewBatchedTransport(a)
		bw = NewBatchedTransport(b)
	}
	ea, err := NewEncryptedTransport(aw, "negotiation test secret 123456", "test", false)
	if err != nil {
		t.Fatal(err)
	}
	eb, err := NewEncryptedTransport(bw, "negotiation test secret 123456", "test", true)
	if err != nil {
		t.Fatal(err)
	}
	pa := PeerParameters{CapabilityIPv4 | CapabilityTCP | CapabilityUDP | CapabilityICMPErrors, 1500}
	ca, err := NewNegotiatedTransport(ea, pa, false)
	if err != nil {
		t.Fatal(err)
	}
	pa.MaxPacketSize = 1280
	cb, err := NewNegotiatedTransport(eb, pa, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ca.Stop(); _ = cb.Stop() })
	return ca, cb, a, b
}

func testIPv4(size int, proto byte) []byte {
	p := make([]byte, size)
	p[0] = 0x45
	p[9] = proto
	binary.BigEndian.PutUint16(p[2:4], uint16(size))
	return p
}

func TestNegotiationAuthenticatedBatchRoundTrip(t *testing.T) {
	a, b, _, _ := negotiatedPair(t, true)
	ch := make(chan []byte, 1)
	b.Receive(func(p []byte) { ch <- append([]byte(nil), p...) })
	errs := make(chan error, 2)
	go func() { errs <- a.Start() }()
	go func() { errs <- b.Start() }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("handshake timeout")
		}
	}
	params, ready := a.PeerParameters()
	if !ready || params.MaxPacketSize != 1280 || params.Capabilities&CapabilityUDP == 0 {
		t.Fatal(params, ready)
	}
	p := testIPv4(1280, 17)
	if err := a.Send(p); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ch:
		if !bytes.Equal(p, got) {
			t.Fatal("packet changed")
		}
	case <-time.After(time.Second):
		t.Fatal("missing UDP")
	}
	if a.Send(testIPv4(1281, 17)) == nil {
		t.Fatal("oversized packet accepted")
	}
	if a.Send(testIPv4(28, 58)) == nil {
		t.Fatal("unknown protocol accepted")
	}
}

func TestNegotiationRequiresFreshEchoAndImmutablePolicy(t *testing.T) {
	a, b, aw, _ := negotiatedPair(t, false)
	aw.drop = true
	if err := a.hello(); err != nil {
		t.Fatal(err)
	}
	// An authentic hello that does not echo this process's challenge is not enough.
	b.receive(a.frameHelloForTest())
	if b.IsConnected() {
		t.Fatal("accepted hello without challenge response")
	}
	aw.drop = false
	if err := a.hello(); err != nil {
		t.Fatal(err)
	}
	if !a.IsConnected() || !b.IsConnected() {
		t.Fatal("handshake incomplete")
	}
	old, _ := b.PeerParameters()
	forged := a.frameHelloForTest()
	binary.BigEndian.PutUint16(forged[74:76], 1400)
	b.receive(forged)
	if got, _ := b.PeerParameters(); got != old {
		t.Fatal("changed established policy")
	}
	forged = a.frameHelloForTest()
	forged[6] ^= 1
	b.receive(forged)
	if b.peer != a.local {
		t.Fatal("replaced established peer")
	}
	// Previously captured hello cannot establish a new receiver instance.
	fresh, _, _, _ := negotiatedPair(t, false)
	fresh.receive(b.frameHelloForTest())
	if fresh.IsConnected() {
		t.Fatal("replayed old session established")
	}
}

func (n *NegotiatedTransport) frameHelloForTest() []byte {
	n.mu.Lock()
	defer n.mu.Unlock()
	p := n.frame(1)
	binary.BigEndian.PutUint32(p[70:74], uint32(n.params.Capabilities))
	binary.BigEndian.PutUint16(p[74:76], uint16(n.params.MaxPacketSize))
	if n.ready {
		p[76] = 1
	}
	return p
}

func TestNegotiationReplayWindowAndCapabilities(t *testing.T) {
	a, b, _, _ := negotiatedPair(t, false)
	b.params.Capabilities = CapabilityIPv4 | CapabilityTCP
	if a.Send(testIPv4(28, 17)) == nil {
		t.Fatal("data before negotiation")
	}
	_ = a.hello()
	if a.Send(testIPv4(28, 17)) == nil {
		t.Fatal("UDP accepted for TCP-only peer")
	}
	count := 0
	b.Receive(func([]byte) { count++ })
	data := func(seq uint64) []byte {
		p := a.frame(2)
		binary.BigEndian.PutUint64(p[70:78], seq)
		return append(p, testIPv4(40, 6)...)
	}
	for _, seq := range []uint64{2, 1, 2, 100, 1, 99, 99, 0} {
		b.receive(data(seq))
	}
	if count != 4 {
		t.Fatalf("replay/reordering delivered %d, want 4", count)
	}
	bad := data(101)
	bad[38] ^= 1
	b.receive(bad)
	if count != 4 {
		t.Fatal("accepted wrong destination session")
	}
}

func TestNegotiationTamperAndLostConfirmation(t *testing.T) {
	a, b, aw, bw := negotiatedPair(t, false)
	aw.drop = true
	bw.drop = true
	_ = a.hello()
	wire := append([]byte(nil), aw.packets[0]...)
	wire[len(wire)-1] ^= 1
	b.EncryptedTransport.Transport.(*negotiationWire).cb(wire)
	if b.peer != ([32]byte{}) {
		t.Fatal("tampered hello accepted")
	}
	// Build the state where A's final confirmation was lost. B's retry must
	// cause a fresh confirmation even though A is already ready.
	a.peer = b.local
	a.remote = b.params
	a.ready = true
	b.peer = a.local
	aw.drop = false
	bw.drop = false
	_ = b.hello()
	if !b.IsConnected() {
		t.Fatal("lost final confirmation never recovered")
	}
}

func TestNegotiationStopInterruptsStart(t *testing.T) {
	a, _, aw, _ := negotiatedPair(t, false)
	aw.drop = true
	done := make(chan error, 1)
	go func() { done <- a.Start() }()
	// Synchronize on the first outgoing hello, not a fixed sleep.
	deadline := time.Now().Add(time.Second)
	for {
		aw.mu.Lock()
		sent := len(aw.packets) > 0
		aw.mu.Unlock()
		if sent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no hello")
		}
		time.Sleep(time.Millisecond)
	}
	if err := a.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("start succeeded after stop")
		}
	case <-time.After(time.Second):
		t.Fatal("stop deadlocked")
	}
}

func TestNegotiationTimeoutHasNoLegacyFallback(t *testing.T) {
	a, _, aw, _ := negotiatedPair(t, false)
	aw.drop = true
	a.handshakeTimeout = 20 * time.Millisecond
	if err := a.Start(); err == nil {
		t.Fatal("silent fallback on absent peer")
	}
	if a.IsConnected() || a.Send(testIPv4(40, 6)) == nil {
		t.Fatal("failed negotiation allowed traffic")
	}
	if _, err := NewNegotiatedTransport(nil, PeerParameters{CapabilityIPv4 | CapabilityTCP, 1500}, false); err == nil {
		t.Fatal("unencrypted negotiation accepted")
	}
	for _, p := range []PeerParameters{{CapabilityIPv4, 1500}, {CapabilityIPv4 | CapabilityTCP, 1279}, {CapabilityIPv4 | CapabilityTCP, 65001}, {CapabilityIPv4 | CapabilityTCP | CapabilityWireV3, 1500}} {
		if validParameters(p) {
			t.Fatal("invalid parameters accepted", p)
		}
	}
}
