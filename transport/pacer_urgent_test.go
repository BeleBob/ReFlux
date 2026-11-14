package transport

import (
	"encoding/binary"
	"testing"
	"time"
)

// udpFlowPacket is a UDP packet of size bytes from source port port; its
// second byte of payload numbers it.
func udpFlowPacket(size int, port uint16, n byte) []byte {
	p := testIPv4(size, 17)
	binary.BigEndian.PutUint16(p[20:], port)
	p[29] = n
	return p
}

// Over a paced carrier that is busy with a download, a packet of a flow
// that has nothing queued - a keystroke over ssh, a chat message - leaves
// in the next frame, ahead of the download's queue. Before, it waited
// behind all of it: seconds over cups.online.
func TestPacedQueueSendsAQuietFlowFirst(t *testing.T) {
	for _, layer := range []string{"batched", "codec"} {
		t.Run(layer, func(t *testing.T) {
			inner := &gatePacer{gate: make(chan struct{}), limit: 1 << 20}
			var l Transport
			if layer == "batched" {
				b := NewBatchedTransport(inner)
				b.Pace(inner)
				b.LimitFrame(4000) // a few packets a frame: the queue spans many
				l = b
			} else {
				c := NewCodecTransport(inner, CodecBatched, false)
				c.LimitFrame(4000)
				l = c
			}
			if err := l.Start(); err != nil {
				t.Fatal(err)
			}
			defer l.Stop()

			for i := 0; i < 40; i++ { // the download: 40 KB queued
				if err := l.Send(udpFlowPacket(1000, 1000, byte(i))); err != nil {
					t.Fatal(err)
				}
			}
			eventually(t, "the layer waiting for the carrier", func() bool { return inner.waits.Load() == 1 })
			if err := l.Send(udpFlowPacket(100, 22, 0)); err != nil { // the keystroke
				t.Fatal(err)
			}
			// The frame being closed already holds the download's first
			// packet; the keystroke must ride with it or the next one.
			for frame := 0; frame < 2; frame++ {
				inner.gate <- struct{}{}
				eventually(t, "a frame", func() bool { return inner.sendCount() == frame+1 })
				pkts, err := decodeBatch(inner.sentAt(frame))
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range pkts {
					if binary.BigEndian.Uint16(p[20:]) == 22 {
						return
					}
				}
			}
			t.Fatal("the quiet flow's packet waits behind the download")
		})
	}
}

// A flow that sends more than urgentBytes at once goes behind the bulk with
// the rest, and its packets still leave in order.
func TestPacedQueueKeepsAFlowsOrder(t *testing.T) {
	inner := &gatePacer{gate: make(chan struct{}), limit: 1 << 20}
	b := NewBatchedTransport(inner)
	b.Pace(inner)
	b.LimitFrame(4000)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	const n = 30
	for i := 0; i < n; i++ {
		if err := b.Send(udpFlowPacket(1000, 1000, byte(i))); err != nil {
			t.Fatal(err)
		}
	}
	close(inner.gate)
	eventually(t, "the queue drained", func() bool { return b.pacing.queuedBytes() == 0 })
	var got []byte
	for i := 0; i < inner.sendCount(); i++ {
		pkts, err := decodeBatch(inner.sentAt(i))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pkts {
			got = append(got, p[29])
		}
	}
	if len(got) != n {
		t.Fatalf("%d packets left, want %d", len(got), n)
	}
	for i := range got {
		if got[i] != byte(i) {
			t.Fatalf("out of order: %v", got)
		}
	}
	if len(b.pacing.flows) != 0 {
		t.Fatalf("flows still counted: %v", b.pacing.flows)
	}
}

// A stripe that cannot take more yet keeps the layer above waiting until it
// is stopped, and counts what its send queue holds.
func TestStripeWaitsForItsQueue(t *testing.T) {
	s := &StripedTransport{done: make(chan struct{})}
	s.snd.init()
	if !s.snd.enqueue(make([]byte, stripePaceBytes)) {
		t.Fatal("enqueue refused")
	}
	stop := make(chan struct{})
	close(stop)
	if s.WaitSend(stop) {
		t.Fatal("WaitSend returned true with a full queue")
	}
	s.snd.dequeue(time.Now())
	if s.snd.queued() != 0 {
		t.Fatalf("queued %d after the dequeue", s.snd.queued())
	}
}

func (f *fakeTransport) sentAt(i int) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[i]
}
