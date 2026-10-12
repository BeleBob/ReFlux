package transport

import (
	"bytes"
	"crypto/rand"
	"sync"
	"testing"
	"time"
)

func TestFillBatchStopsBeforeThePacketPastTheLimit(t *testing.T) {
	queue := make(chan []byte, 8)
	for _, n := range []int{400, 400, 400} {
		queue <- make([]byte, n)
	}
	// 2+400 for each packet: two fit in 1000 bytes, the third is carried.
	batch, size, carry, stopped := fillBatch(queue, nil, make([]byte, 100), 8192, 64, 0, 1000)
	if stopped || len(batch) != 3 || size != 102+2*402 {
		t.Fatalf("batch of %d packets, %d bytes (stopped %v), want 3 packets, %d bytes", len(batch), size, stopped, 102+2*402)
	}
	if len(carry) != 400 {
		t.Fatalf("carry = %d bytes, want the third 400-byte packet", len(carry))
	}
	if len(queue) != 0 {
		t.Fatalf("%d packets left queued", len(queue))
	}

	// Without a limit the same packets make one batch.
	for _, n := range []int{400, 400, 400} {
		queue <- make([]byte, n)
	}
	batch, _, carry, _ = fillBatch(queue, nil, make([]byte, 100), 8192, 64, 0, 0)
	if len(batch) != 4 || carry != nil {
		t.Fatalf("no limit: batch of %d packets, carry %d bytes, want 4 and none", len(batch), len(carry))
	}
}

// limitedWire is a negotiationWire that, like Mail.ru since 2026-10-12,
// accepts frames up to a size only; it records the biggest it was handed.
type limitedWire struct {
	negotiationWire
	limit   int
	sizeMu  sync.Mutex
	biggest int
}

func (w *limitedWire) MaxFrame() int { return w.limit }

func (w *limitedWire) Send(p []byte) error {
	w.sizeMu.Lock()
	w.biggest = max(w.biggest, len(p))
	w.sizeMu.Unlock()
	return w.negotiationWire.Send(p)
}

// A burst over a carrier with a frame limit goes out in frames under it, and
// every packet arrives in order. Before, a burst made batches of up to 8 KB,
// and Mail.ru dropped the connection on each.
func TestSessionKeepsFramesUnderTheCarrierLimit(t *testing.T) {
	const limit = 3000
	a := &limitedWire{limit: limit}
	b := &limitedWire{limit: limit}
	a.peer, b.peer = &b.negotiationWire, &a.negotiationWire

	params := PeerParameters{Capabilities: CapabilityIPv4 | CapabilityTCP | CapabilityUDP, MaxPacketSize: 1500}
	client, err := NewSession(params, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.AddTransport("primary", a, testSessionSecret, testSessionCtx, 100); err != nil {
		t.Fatal(err)
	}
	exit, err := NewSession(params, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := exit.AddTransport("primary", b, testSessionSecret, testSessionCtx, 100); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Stop(); _ = exit.Stop() })

	var mu sync.Mutex
	var got [][]byte
	exit.Receive(func(p []byte) {
		mu.Lock()
		got = append(got, append([]byte(nil), p...))
		mu.Unlock()
	})
	errs := make(chan error, 2)
	go func() { errs <- client.Start() }()
	go func() { errs <- exit.Start() }()
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

	// Random payloads do not compress, so the batches stay as big as the
	// packets in them.
	var sent [][]byte
	for i := 0; i < 40; i++ {
		p := testIPv4(1200+i*7, 17)
		rand.Read(p[20:])
		sent = append(sent, p)
		if err := client.Send(p); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "every packet delivered", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == len(sent)
	})
	mu.Lock()
	for i := range sent {
		if !bytes.Equal(got[i], sent[i]) {
			t.Fatalf("packet %d changed or out of order", i)
		}
	}
	mu.Unlock()
	a.sizeMu.Lock()
	defer a.sizeMu.Unlock()
	if a.biggest > limit {
		t.Fatalf("the carrier was handed a %d-byte frame, above its %d-byte limit", a.biggest, limit)
	}
}
