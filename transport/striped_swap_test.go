package transport

import (
	"sync"
	"testing"
	"time"
)

// stopLane is a delayLane that delivers nothing once stopped.
type stopLane struct{ *delayLane }

func (l *stopLane) Send(b []byte) error {
	if !l.alive.Load() {
		return nil
	}
	return l.delayLane.Send(b)
}

func newStopPair() (*stopLane, *stopLane) {
	a, b := newDelayPair(10 * time.Millisecond)
	return &stopLane{a}, &stopLane{b}
}

// A lane whose carrier is replaced by a new object keeps its index: frames
// go out on the new one, what it receives comes in as that lane's, and the
// old one is stopped.
func TestStripeSwapLaneCarriesOnTheNewLane(t *testing.T) {
	const lanes = 2
	var aLanes, bLanes []Transport
	var aEnds, bEnds []*stopLane
	for i := 0; i < lanes; i++ {
		a, b := newStopPair()
		aLanes, bLanes = append(aLanes, a), append(bLanes, b)
		aEnds, bEnds = append(aEnds, a), append(bEnds, b)
	}
	A, B := NewStripedTransport(aLanes), NewStripedTransport(bLanes)
	var mu sync.Mutex
	got := map[byte]bool{}
	B.Receive(func(b []byte) { mu.Lock(); got[b[0]] = true; mu.Unlock() })
	if err := A.Start(); err != nil {
		t.Fatal(err)
	}
	if err := B.Start(); err != nil {
		t.Fatal(err)
	}
	defer A.Stop()
	defer B.Stop()

	// Lane 1 dies on both ends; new objects take its place.
	_ = aEnds[1].Stop()
	_ = bEnds[1].Stop()
	na, nb := newStopPair()
	_ = na.Start()
	_ = nb.Start()
	A.SwapLane(1, na)
	B.SwapLane(1, nb)
	if A.laneAt(1) != na || A.LaneCount() != lanes {
		t.Fatal("lane 1 is not the new lane")
	}

	// Lane 0 goes too: whatever arrives now came over the new lane 1.
	_ = aEnds[0].Stop()
	_ = bEnds[0].Stop()
	A.lanesMu.Lock()
	ready := append([]time.Time(nil), A.readyAt...)
	ready[1] = time.Time{} // no warm-up in a test
	A.readyAt = ready
	A.lanesMu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for i := 0; ; i++ {
		_ = A.Send([]byte{byte(i), 1, 2, 3})
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d frames arrived over the swapped lane", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if aEnds[1].IsConnected() {
		t.Error("the replaced lane was not stopped")
	}
}

// A lane offered after the stripe stopped, or for an index it does not
// have, is stopped and not used.
func TestStripeSwapLaneRefusesWhatItCannotUse(t *testing.T) {
	a, _ := newDelayPair(time.Millisecond)
	s := NewStripedTransport([]Transport{a})
	extra, _ := newDelayPair(time.Millisecond)
	_ = extra.Start()
	s.SwapLane(3, extra)
	if extra.IsConnected() || s.laneAt(0) != a {
		t.Fatal("a lane for a missing index was used")
	}
	_ = s.Stop()
	late, _ := newDelayPair(time.Millisecond)
	_ = late.Start()
	s.SwapLane(0, late)
	if late.IsConnected() || s.laneAt(0) != a {
		t.Fatal("a lane offered after Stop was used")
	}
}
