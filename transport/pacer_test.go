package transport

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gatePacer is a carrier that may send one frame per value on gate (all of
// them once gate is closed).
type gatePacer struct {
	fakeTransport
	gate  chan struct{}
	limit int
	waits atomic.Int32
}

func (g *gatePacer) WaitSend(stop <-chan struct{}) bool {
	g.waits.Add(1)
	select {
	case <-g.gate:
		return true
	case <-stop:
		return false
	}
}

func (g *gatePacer) QueueBytes() int { return g.limit }

// Over a carrier that may send only now and then (Mail.ru allows a
// connection about five messages a second), what queues while it waits
// goes out in one frame. Before, each packet left in a frame of its own as
// it came, and traffic spent the carrier's allowance packet by packet.
func TestPacedLayersCloseABatchOnlyWhenTheCarrierCanSend(t *testing.T) {
	for _, layer := range []string{"batched", "codec"} {
		t.Run(layer, func(t *testing.T) {
			inner := &gatePacer{gate: make(chan struct{}), limit: 1 << 20}
			var l Transport
			if layer == "batched" {
				b := NewBatchedTransport(inner)
				b.Pace(inner)
				l = b
			} else {
				// A codec straight over a pacer (keyless classic mode)
				// paces by itself.
				l = NewCodecTransport(inner, CodecBatched, false)
			}
			if err := l.Start(); err != nil {
				t.Fatal(err)
			}
			defer l.Stop()

			const n = 10
			for i := 0; i < n; i++ {
				if err := l.Send([]byte{byte(i), 0xAA, 0xBB}); err != nil {
					t.Fatal(err)
				}
			}
			eventually(t, "the layer waiting for the carrier", func() bool { return inner.waits.Load() == 1 })
			time.Sleep(20 * time.Millisecond)
			if c := inner.sendCount(); c != 0 {
				t.Fatalf("%d frames went out before the carrier could send", c)
			}
			inner.gate <- struct{}{}
			eventually(t, "one frame", func() bool { return inner.sendCount() == 1 })
			pkts, err := decodeBatch(inner.firstSent())
			if err != nil {
				t.Fatal(err)
			}
			if len(pkts) != n {
				t.Fatalf("the frame carries %d packets, want all %d queued", len(pkts), n)
			}
		})
	}
}

// A paced queue holds about a second of bulk (QueueBytes) and refuses more
// at once, so TCP slows down rather than wait in a queue that takes seconds
// to drain. One flow may take only half of it, so a download cannot keep a
// page opened meanwhile out; and small packets (handshakes,
// acknowledgements, DNS) always get in: refused, a new connection waits
// out retransmissions, seconds each.
func TestPacedQueueSharesItsRoomBetweenFlows(t *testing.T) {
	inner := &gatePacer{gate: make(chan struct{}), limit: 4000}
	b := NewBatchedTransport(inner)
	b.Pace(inner)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()

	// A 1000-byte UDP packet of the flow from source port port.
	bulk := func(port byte) []byte {
		p := testIPv4(1000, 17)
		p[21] = port
		return p
	}
	for _, c := range []struct {
		flow byte
		ok   bool
		why  string
	}{
		{1, true, "first packet of a flow"},
		{1, true, "a flow up to half the queue"},
		{1, false, "a flow past half the queue"},
		{2, true, "another flow while the queue has room"},
		{2, true, "the other flow up to its half"},
		{3, false, "a third flow when the queue is full"},
	} {
		if err := b.Send(bulk(c.flow)); (err == nil) != c.ok {
			t.Fatalf("%s: Send error %v", c.why, err)
		}
	}
	if err := b.Send(testIPv4(60, 6)); err != nil {
		t.Fatalf("a small packet was refused: %v", err)
	}

	close(inner.gate)
	eventually(t, "the queue drained", func() bool { return b.pacing.queuedBytes() == 0 })
	if err := b.Send(bulk(1)); err != nil {
		t.Fatalf("bulk refused once the queue drained: %v", err)
	}
}

// pacedWire is a limitedWire that sends at most one frame per every.
type pacedWire struct {
	limitedWire
	every  time.Duration
	lastMu sync.Mutex
	last   time.Time
	frames atomic.Int32
}

func (w *pacedWire) WaitSend(stop <-chan struct{}) bool {
	w.lastMu.Lock()
	next := w.last.Add(w.every)
	w.lastMu.Unlock()
	select {
	case <-time.After(time.Until(next)):
		return true
	case <-stop:
		return false
	}
}

func (w *pacedWire) QueueBytes() int { return 1 << 20 }

func (w *pacedWire) Send(p []byte) error {
	w.lastMu.Lock()
	w.last = time.Now()
	w.lastMu.Unlock()
	w.frames.Add(1)
	return w.limitedWire.Send(p)
}

// A Session over a pacer sends fewer, fuller frames: packets that come
// every 10 ms over a carrier that sends every 50 ms share frames, in order.
func TestSessionWaitsForAPacedCarrier(t *testing.T) {
	a := &pacedWire{limitedWire: limitedWire{limit: 3060}, every: 50 * time.Millisecond}
	b := &pacedWire{limitedWire: limitedWire{limit: 3060}, every: 50 * time.Millisecond}
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

	before := a.frames.Load()
	var sent [][]byte
	for i := 0; i < 40; i++ {
		p := testIPv4(100, 17)
		p[30] = byte(i)
		sent = append(sent, p)
		if err := client.Send(p); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
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
	// 40 packets over about 400 ms: about 8 frames, a few more for pings.
	if n := a.frames.Load() - before; n > 15 {
		t.Fatalf("%d frames for 40 packets over a carrier that sends every 50 ms", n)
	}
}
