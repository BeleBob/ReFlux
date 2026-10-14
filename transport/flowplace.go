package transport

import (
	"sync"
	"time"
)

// flowletGap is how long a flow must have been quiet before it may move to
// another carrier: longer than any carrier keeps a packet (a paced Mail.ru
// queue holds two seconds), so its earlier packets are through and the move
// cannot reorder it.
const flowletGap = 3 * time.Second

// flowForget drops the pin of a flow quiet for this long.
const flowForget = time.Minute

// flowWeight is what one active flow adds to a carrier's load when a new
// flow is placed, so carriers with empty queues take new flows in turn.
const flowWeight = 4 << 10

// maxPinnedFlows bounds the table; past it, new flows are placed without a
// pin until quiet ones are forgotten.
const maxPinnedFlows = 4096

// backlogPacketBytes is what a queued packet counts for over a carrier that
// does not pace, where the queue is counted in packets, not bytes.
const backlogPacketBytes = 1500

// flowPlacement keeps each flow on the carrier it was placed on (pick). The
// zero value is ready.
type flowPlacement struct {
	mu    sync.Mutex
	flows map[flowKeyBytes]flowPin
}

type flowPin struct {
	link string
	last time.Time
}

// pick returns the carrier for packet p among top, the live carriers of the
// highest priority. A flow keeps its carrier while it sends, so its packets
// stay in order. A new flow, one back from a pause of flowletGap, or one
// whose carrier left the group goes to the least loaded carrier: the fewest
// bytes waiting to be sent, each active flow counting flowWeight.
//
// Flows used to be placed by a hash of their addresses, so two downloads
// could share one carrier while another stood idle. Since 2026-10-14 a
// Mail.ru document carries about 9 KB/s, and that halved both.
func (fp *flowPlacement) pick(top []*transportLink, p []byte, now time.Time) *transportLink {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if fp.flows == nil {
		// One carrier so far: there is no order to keep. Once there were
		// several, flows are pinned even while one is left, so the ones
		// that moved to it stay when the others return.
		if len(top) == 1 {
			return top[0]
		}
		fp.flows = make(map[flowKeyBytes]flowPin)
	}
	key := extractFlowKeyBytes(p)
	if pin, ok := fp.flows[key]; ok && now.Sub(pin.last) < flowletGap {
		for _, l := range top {
			if l.name == pin.link {
				pin.last = now
				fp.flows[key] = pin
				return l
			}
		}
	}
	active := fp.activeLocked(now)
	best, bestLoad := top[0], int64(-1)
	for _, l := range top {
		load := l.batched.Backlog() + int64(active[l.name])*flowWeight
		if bestLoad < 0 || load < bestLoad {
			best, bestLoad = l, load
		}
	}
	if _, ok := fp.flows[key]; ok || len(fp.flows) < maxPinnedFlows {
		fp.flows[key] = flowPin{link: best.name, last: now}
	}
	return best
}

// activeLocked counts the flows each carrier sent within flowletGap, and
// forgets the ones quiet for flowForget.
func (fp *flowPlacement) activeLocked(now time.Time) map[string]int {
	active := make(map[string]int)
	for k, pin := range fp.flows {
		switch age := now.Sub(pin.last); {
		case age < flowletGap:
			active[pin.link]++
		case age > flowForget:
			delete(fp.flows, k)
		}
	}
	return active
}

// flowCounts is how many flows each carrier sent within flowletGap.
func (fp *flowPlacement) flowCounts(now time.Time) map[string]int {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return fp.activeLocked(now)
}

// Backlog is how many bytes wait to be batched: counted over a paced
// carrier, estimated from the queue length over one that is not (its queue
// drains as fast as the carrier takes frames).
func (b *BatchedTransport) Backlog() int64 {
	if b.pacing.pacer != nil {
		return b.pacing.queuedBytes()
	}
	return int64(len(b.queue)) * backlogPacketBytes
}
