package transport

import (
	"bytes"
	"sync"

	"github.com/p1neappleXpress/OpenFlux/transport/control"
)

// Pacer is a carrier whose service limits how often a connection may send:
// since 2026-10-14 Mail.ru's document server drops a connection that writes
// more than about five messages a second beyond a burst ("access deny").
// The batching layers above such a carrier close a batch only once the
// carrier can send it at once, so each frame carries everything that queued
// meanwhile, and they keep a short queue: one that takes seconds to drain
// delays every flow by as much and stalls TCP in retransmission backoff.
type Pacer interface {
	// WaitSend blocks until the carrier can write a frame without waiting.
	// It returns false once stop is closed or the carrier stops.
	WaitSend(stop <-chan struct{}) bool
	// QueueBytes is how many bytes of bulk packets may wait above the
	// carrier, a second or two of what it sends; one flow may hold half.
	QueueBytes() int
}

// smallPacket is the size up to which a packet always joins a paced queue:
// TCP handshakes and acknowledgements, DNS and Session control messages
// (with their 78-byte envelope). Dropping them for the bulk ahead of them
// would make new connections wait for retransmissions, seconds each.
const smallPacket = 300

// pacing is the queue discipline of a batching layer above a Pacer; the
// zero value (no pacer) admits everything and never waits.
//
// A bulk packet joins the queue only while its flow holds less than half
// of QueueBytes and the queue less than all of it: one download cannot
// fill the queue, so a page opened meanwhile gets its packets in, and
// waits behind half the queue of the download rather than all of it.
type pacing struct {
	pacer Pacer
	limit int64

	mu sync.Mutex
	// queued counts the bytes of packets queued and not yet batched, in
	// all and by flow.
	queued int64
	flows  map[uint64]int64
}

// pace sets the carrier's pacer; call it before the layer starts.
func (p *pacing) pace(pacer Pacer) {
	p.pacer = pacer
	p.limit = int64(pacer.QueueBytes())
	p.flows = make(map[uint64]int64)
}

// admit reports whether pkt may join the queue, and counts it if so; a
// caller that then fails to queue it calls done(pkt).
func (p *pacing) admit(pkt []byte) bool {
	if p.pacer == nil {
		return true
	}
	n, flow := int64(len(pkt)), flowOf(pkt)
	p.mu.Lock()
	defer p.mu.Unlock()
	if n > smallPacket && (p.flows[flow]+n > p.limit/2 || p.queued+n > p.limit) {
		return false
	}
	p.queued += n
	p.flows[flow] += n
	return true
}

// done uncounts packets that left the queue.
func (p *pacing) done(packets ...[]byte) {
	if p.pacer == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pkt := range packets {
		n, flow := int64(len(pkt)), flowOf(pkt)
		p.queued -= n
		if p.flows[flow] -= n; p.flows[flow] <= 0 {
			delete(p.flows, flow)
		}
	}
}

// queuedBytes is what the queue holds.
func (p *pacing) queuedBytes() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.queued
}

// flowOf keys a queued packet by its IPv4 flow: a Session's data packet
// carries it after the envelope, the classic codec's is the packet itself.
// Control and hello envelopes are flow 0.
func flowOf(pkt []byte) uint64 {
	if len(pkt) >= control.EnvelopeSize && bytes.Equal(pkt[:4], control.Magic[:]) {
		if control.Kind(pkt[4]) != control.KindIPv4 {
			return 0
		}
		pkt = pkt[control.EnvelopeSize:]
	}
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return 0
	}
	return flowHashBytes(extractFlowKeyBytes(pkt))
}

// wait blocks until the carrier can send; false means stop.
func (p *pacing) wait(stop <-chan struct{}) bool {
	return p.pacer == nil || p.pacer.WaitSend(stop)
}

// pacerOf returns raw as a Pacer, or nil when it does not limit its rate.
func pacerOf(raw Transport) Pacer {
	if p, ok := raw.(Pacer); ok {
		return p
	}
	return nil
}
