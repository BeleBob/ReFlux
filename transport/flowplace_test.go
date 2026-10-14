package transport

import (
	"encoding/binary"
	"sync/atomic"
	"testing"
	"time"
)

// placeLinks builds carriers for flowPlacement.pick; each paces, so its
// queue counts bytes as over Mail.ru.
func placeLinks(names ...string) []*transportLink {
	var out []*transportLink
	for _, n := range names {
		inner := &gatePacer{gate: make(chan struct{}), limit: 1 << 20}
		b := NewBatchedTransport(inner)
		b.Pace(inner)
		out = append(out, &transportLink{name: n, batched: b})
	}
	return out
}

// backlog queues n bytes on l without sending them.
func backlog(t *testing.T, l *transportLink, n int) {
	t.Helper()
	if !l.batched.pacing.admit(make([]byte, n)) {
		t.Fatalf("%s refused %d bytes", l.name, n)
	}
}

// flowPacket is a full-size TCP packet of the flow from local port port.
func flowPacket(port uint16) []byte {
	p := testIPv4(1400, 6)
	copy(p[12:], []byte{10, 0, 0, 2})
	copy(p[16:], []byte{93, 184, 216, 34})
	binary.BigEndian.PutUint16(p[20:], port)
	binary.BigEndian.PutUint16(p[22:], 443)
	return p
}

// collidingPorts returns n local ports whose flows the former address hash
// put on the same one of carriers carriers.
func collidingPorts(n, carriers int) []uint16 {
	var ports []uint16
	var want uint64
	for port := uint16(40000); len(ports) < n; port++ {
		h := flowHashBytes(extractFlowKeyBytes(flowPacket(port))) % uint64(carriers)
		if len(ports) == 0 {
			want = h
		}
		if h == want {
			ports = append(ports, port)
		}
	}
	return ports
}

// Three downloads over a client's three documents get one document each.
// Placed by a hash of their addresses, they could all share one document
// (about 9 KB/s over Mail.ru) while two stood idle.
func TestFlowPlacementSpreadsFlowsTheHashStacked(t *testing.T) {
	top := placeLinks("mailru", "mailru-2", "mailru-3")
	var fp flowPlacement
	now := time.Now()
	used := map[string]bool{}
	for _, port := range collidingPorts(3, 3) {
		used[fp.pick(top, flowPacket(port), now).name] = true
	}
	if len(used) != 3 {
		t.Fatalf("three flows went to %v, want one carrier each", used)
	}
}

// A flow stays on its carrier while it sends, however loaded that carrier
// gets: moving it would reorder its packets.
func TestFlowPlacementKeepsASendingFlowOnItsCarrier(t *testing.T) {
	top := placeLinks("a", "b")
	var fp flowPlacement
	now := time.Now()
	first := fp.pick(top, flowPacket(1000), now)
	backlog(t, first, 20<<10)
	for i := 1; i <= 10; i++ {
		now = now.Add(flowletGap / 2)
		if got := fp.pick(top, flowPacket(1000), now); got != first {
			t.Fatalf("packet %d of the flow moved from %s to %s", i, first.name, got.name)
		}
	}
	if got := fp.pick(top, flowPacket(2000), now); got == first {
		t.Fatalf("a new flow went to the loaded carrier %s", got.name)
	}
}

// A flow quiet for flowletGap has nothing left in flight, so it starts
// again on the least loaded carrier.
func TestFlowPlacementMovesAFlowAfterAPause(t *testing.T) {
	top := placeLinks("a", "b")
	var fp flowPlacement
	now := time.Now()
	first := fp.pick(top, flowPacket(1000), now)
	backlog(t, first, 20<<10)
	if got := fp.pick(top, flowPacket(1000), now.Add(flowletGap)); got == first {
		t.Fatalf("after a pause the flow stayed on the loaded carrier %s", got.name)
	}
}

// A new flow goes where the least waits: a queue counts against a carrier
// along with its active flows.
func TestFlowPlacementWeighsQueuesAndFlows(t *testing.T) {
	top := placeLinks("a", "b")
	var fp flowPlacement
	now := time.Now()
	backlog(t, top[0], 20<<10)
	for i, want := range []string{"b", "b", "b", "b", "b", "a"} {
		// b: 0, 4, 8, 12, 16, then 20 KiB of flows against a's 20 KiB queue.
		if got := fp.pick(top, flowPacket(uint16(1000+i)), now); got.name != want {
			t.Fatalf("flow %d went to %s, want %s", i, got.name, want)
		}
	}
}

// When a carrier stops reaching the peer, its flows move on their next
// packet; when it is back, new flows find it empty and go there.
func TestFlowPlacementFollowsCarriersLeavingAndReturning(t *testing.T) {
	top := placeLinks("a", "b")
	var fp flowPlacement
	now := time.Now()
	var onA []uint16
	for port := uint16(1000); len(onA) < 2; port++ {
		if fp.pick(top, flowPacket(port), now).name == "a" {
			onA = append(onA, port)
		}
	}
	for _, port := range onA {
		if got := fp.pick(top[1:], flowPacket(port), now); got.name != "b" {
			t.Fatalf("flow %d stayed on %s after it left", port, got.name)
		}
	}
	now = now.Add(time.Second)
	for _, port := range onA {
		if got := fp.pick(top, flowPacket(port), now); got.name != "b" {
			t.Fatalf("flow %d moved back to %s while sending", port, got.name)
		}
	}
	if got := fp.pick(top, flowPacket(9000), now); got.name != "a" {
		t.Fatalf("a new flow went to %s, want the returned carrier a", got.name)
	}
}

// Pins of flows quiet for flowForget are dropped, and the table is bounded.
func TestFlowPlacementForgetsQuietFlows(t *testing.T) {
	top := placeLinks("a", "b")
	var fp flowPlacement
	now := time.Now()
	for i := 0; i < maxPinnedFlows+100; i++ {
		fp.pick(top, flowPacket(uint16(i)), now)
	}
	if n := len(fp.flows); n != maxPinnedFlows {
		t.Fatalf("%d flows pinned, want the bound %d", n, maxPinnedFlows)
	}
	fp.pick(top, flowPacket(60000), now.Add(flowForget+time.Second))
	if n := len(fp.flows); n != 1 {
		t.Fatalf("%d flows pinned after the others went quiet, want 1", n)
	}
	if c := fp.flowCounts(now.Add(flowForget + time.Second)); c["a"]+c["b"] != 1 {
		t.Fatalf("active flows %v, want the one just placed", c)
	}
}

// Through a Session: flows the former hash stacked on one of three
// carriers of equal priority each get their own, and all of it arrives.
func TestSessionSpreadsFlowsOverEqualCarriers(t *testing.T) {
	client, exit, cw, _ := linkedSessionsWith(t, func(int) int { return 100 }, "mailru", "mailru-2", "mailru-3")
	var got atomic.Int32
	exit.Receive(func([]byte) { got.Add(1) })
	startPair(t, client, exit)
	eventually(t, "all three carriers heard", func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return len(client.liveLinksLocked()) == 3
	})
	before := map[string]int{}
	for name, w := range cw {
		before[name] = sentCount(w)
	}

	const perFlow = 20
	for i := 0; i < perFlow; i++ {
		for _, port := range collidingPorts(3, 3) {
			if err := client.Send(flowPacket(port)); err != nil {
				t.Fatal(err)
			}
		}
	}
	eventually(t, "every packet delivered", func() bool { return got.Load() == 3*perFlow })
	counts := client.placement.flowCounts(time.Now())
	if len(counts) != 3 {
		t.Fatalf("flows per carrier %v, want one on each of three", counts)
	}
	for name, w := range cw {
		if sentCount(w) == before[name] {
			t.Fatalf("carrier %s carried none of the flows", name)
		}
	}
}
