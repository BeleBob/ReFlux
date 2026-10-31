package transport

import (
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

// reviveLane is one end of an in-memory lane that can stop delivering while
// it stays up, the way a cups.online room does when the server keeps taking
// what is published and forwards none of it: Send succeeds and nothing
// arrives. ReviveLane - what the carrier's reconnect does for real - puts
// delivery back.
type reviveLane struct {
	peer *reviveLane
	mu   sync.Mutex
	cb   func([]byte)
	mute bool
	revs []bool // one entry per ReviveLane call: whether it asked for hard
	// onRevive runs after a revival, so a test can model a link that a
	// reconnect does not fix.
	onRevive func(hard bool)
	once     sync.Once
	wire     chan []byte
}

func (l *reviveLane) Start() error {
	l.once.Do(func() {
		l.wire = make(chan []byte, 1<<16)
		go func() {
			for b := range l.wire {
				time.Sleep(time.Millisecond)
				l.peer.mu.Lock()
				cb := l.peer.cb
				l.peer.mu.Unlock()
				if cb != nil {
					cb(b)
				}
			}
		}()
	})
	return nil
}

func (l *reviveLane) Stop() error { return nil }

func (l *reviveLane) Send(b []byte) error {
	l.mu.Lock()
	mute := l.mute
	l.mu.Unlock()
	if mute {
		return nil // taken and never delivered
	}
	l.wire <- append([]byte(nil), b...)
	return nil
}

func (l *reviveLane) Receive(cb func([]byte)) { l.mu.Lock(); l.cb = cb; l.mu.Unlock() }
func (l *reviveLane) IsConnected() bool       { return true }
func (l *reviveLane) Stats() TransportStats   { return TransportStats{Connected: true} }

func (l *reviveLane) setMute(v bool) { l.mu.Lock(); l.mute = v; l.mu.Unlock() }

// ReviveLane is the carrier putting its own link back up.
func (l *reviveLane) ReviveLane(hard bool) {
	l.mu.Lock()
	l.revs, l.mute = append(l.revs, hard), false
	after := l.onRevive
	l.mu.Unlock()
	if after != nil {
		after(hard)
	}
}

func (l *reviveLane) revivals() []bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]bool(nil), l.revs...)
}

// A lane that keeps its link up and stops delivering is found from the acks -
// the peer says what it took off each lane - and asked to come back. Without
// that the stripe runs on the lanes that work for the rest of the session: it
// still delivers every frame (the pieces go again elsewhere), so nothing but
// the per-lane figures shows the loss.
func TestStripedRevivesALaneThatStopsCarrying(t *testing.T) {
	defer shortRevival(t)()

	const lanes = 3
	var aLanes, bLanes []Transport
	var aEnds []*reviveLane
	for i := 0; i < lanes; i++ {
		a, b := &reviveLane{}, &reviveLane{}
		a.peer, b.peer = b, a
		aLanes, bLanes = append(aLanes, a), append(bLanes, b)
		aEnds = append(aEnds, a)
	}
	A, B := NewStripedTransport(aLanes), NewStripedTransport(bLanes)
	var mu sync.Mutex
	var got int
	B.Receive(func([]byte) { mu.Lock(); got++; mu.Unlock() })
	if err := A.Start(); err != nil {
		t.Fatal(err)
	}
	if err := B.Start(); err != nil {
		t.Fatal(err)
	}
	defer A.Stop()
	defer B.Stop()

	// A steady stream, so every lane is filled and the one that delivers
	// nothing stands out.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			p := make([]byte, 400)
			binary.BigEndian.PutUint32(p, uint32(i))
			_ = A.Send(p)
		}
	}()

	waitUntil(t, 5*time.Second, "every lane to carry", func() bool {
		for i := 0; i < lanes; i++ {
			if B.laneRecv[i].Load() == 0 {
				return false
			}
		}
		return true
	})

	// Lane 0 goes one-way: it takes A's pieces and delivers none.
	aEnds[0].setMute(true)
	time.Sleep(50 * time.Millisecond)
	before := B.laneRecv[0].Load()

	waitUntil(t, 10*time.Second, "the one-way lane to be revived", func() bool {
		return len(aEnds[0].revivals()) > 0
	})
	if hard := aEnds[0].revivals()[0]; hard {
		t.Error("the first revival must be the cheap one (a reconnect), not a re-entry")
	}
	waitUntil(t, 5*time.Second, "the revived lane to carry again", func() bool {
		return B.laneRecv[0].Load() > before
	})
	for i := 1; i < lanes; i++ {
		if r := aEnds[i].revivals(); len(r) > 0 {
			t.Errorf("lane %d was carrying and was revived %d time(s) anyway", i, len(r))
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if got == 0 {
		t.Error("no frame arrived at all")
	}
}

// What the watchdog decides, with both sides' counts fed to it directly: a
// lane this side keeps filling and the peer hears nothing from is reconnected
// and then re-entered, while a lane that carries and a lane nothing goes on
// are both left alone. Driving a real sender into those three states at once
// takes a whole session; the decision is the part worth pinning down.
func TestStripedRevivalEscalatesToAReEntry(t *testing.T) {
	defer shortRevival(t)()

	const lanes = 3
	var ends []*reviveLane
	var ls []Transport
	for i := 0; i < lanes; i++ {
		l, far := &reviveLane{}, &reviveLane{}
		l.peer, far.peer = far, l
		ends, ls = append(ends, l), append(ls, l)
	}
	A := NewStripedTransport(ls)
	A.Receive(func([]byte) {})
	if err := A.Start(); err != nil {
		t.Fatal(err)
	}
	defer A.Stop()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		var put, carried uint32
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			// Pieces go on lanes 0 and 1 and none on lane 2; the peer takes
			// them off lane 1 only.
			put, carried = put+4, carried+4
			A.snd.mu.Lock()
			A.snd.laneTx[0], A.snd.laneTx[1] = uint64(put), uint64(put)
			A.snd.mu.Unlock()
			A.peerGotMu.Lock()
			A.peerGot = []uint32{0, carried, 0}
			A.peerGotMu.Unlock()
		}
	}()

	waitUntil(t, 10*time.Second, "the revival to escalate", func() bool {
		for _, hard := range ends[0].revivals() {
			if hard {
				return true
			}
		}
		return false
	})
	if first := ends[0].revivals()[0]; first {
		t.Error("the first revival must be a reconnect, not a re-entry")
	}
	why := map[int]string{1: "was carrying", 2: "had nothing put on it"}
	for i := 1; i < lanes; i++ {
		if r := ends[i].revivals(); len(r) > 0 {
			t.Errorf("lane %d was revived %d time(s): it %s", i, len(r), why[i])
		}
	}
}

// A peer from before the counts says nothing about its lanes, and then no
// lane is ever revived - the stripe behaves as it did.
func TestStripedLeavesLanesAloneWithoutCounts(t *testing.T) {
	defer shortRevival(t)()

	var ends []*reviveLane
	var ls []Transport
	for i := 0; i < 2; i++ {
		l, far := &reviveLane{}, &reviveLane{}
		l.peer, far.peer = far, l
		ends, ls = append(ends, l), append(ls, l)
	}
	A := NewStripedTransport(ls)
	A.Receive(func([]byte) {})
	if err := A.Start(); err != nil {
		t.Fatal(err)
	}
	defer A.Stop()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		var put uint32
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			put += 4
			A.snd.mu.Lock()
			A.snd.laneTx[0], A.snd.laneTx[1] = uint64(put), uint64(put)
			A.snd.mu.Unlock()
		}
	}()
	time.Sleep(4 * laneQuietFor)
	for i := range ends {
		if r := ends[i].revivals(); len(r) > 0 {
			t.Errorf("lane %d was revived %d time(s) on no word from the peer", i, len(r))
		}
	}
}

// An ack carries the counts behind the runs it names, where a peer from
// before this change never looks.
func TestAckCarriesLaneCounts(t *testing.T) {
	lanes := []Transport{&reviveLane{}, &reviveLane{}, &reviveLane{}}
	s := NewStripedTransport(lanes)
	s.laneRecv[0].Store(7)
	s.laneRecv[2].Store(1 << 20)
	plain := []byte{stripeAckMagic, 0, 0, 0, 9, 0, 0, 0, 12, 0}
	ack := s.withLaneCounts(plain)
	if len(ack) <= len(plain) {
		t.Fatal("the ack carries no counts")
	}
	if string(ack[:len(plain)]) != string(plain) {
		t.Error("the ack itself changed")
	}
	// What a peer that knows about them reads back.
	peer := NewStripedTransport(lanes)
	peer.takeLaneCounts(ack)
	got := peer.peerGotCounts(3)
	if len(got) != 3 || got[0] != 7 || got[1] != 0 || got[2] != 1<<20 {
		t.Fatalf("read back %v", got)
	}
	// A plain ack, and one whose counts are for another number of lanes,
	// leave nothing behind.
	peer.takeLaneCounts(plain)
	if got := peer.peerGotCounts(2); got != nil {
		t.Errorf("counts for 3 lanes were taken for 2: %v", got)
	}
	if n := len(peer.peerGotCounts(3)); n != 3 {
		t.Errorf("a plain ack dropped the counts: %d", n)
	}
}

// shortRevival makes the watchdog's waits test-sized.
func shortRevival(t *testing.T) func() {
	t.Helper()
	check, quiet, cooldown, busy := laneQuietCheck, laneQuietFor, laneQuietCooldown, laneQuietBusy
	// A lane the sender has given up on still gets a probe piece every
	// 100 ms, so a check window must be long enough to see one.
	laneQuietCheck = 150 * time.Millisecond
	laneQuietFor = 450 * time.Millisecond
	laneQuietCooldown = 300 * time.Millisecond
	laneQuietBusy = 1
	return func() {
		laneQuietCheck, laneQuietFor, laneQuietCooldown, laneQuietBusy = check, quiet, cooldown, busy
	}
}

func waitUntil(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waited %s for %s", within, what)
}
