package transport

import (
	"encoding/binary"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cupsLikeOptions are the cups.online lanes' stripe options
// (cupsonline.stripeOptions), for tests in this package.
var cupsLikeOptions = StripeOptions{
	PieceBytes: 8192/4 - 2 - 10, DeadAfter: 3 * time.Second, NoDups: true,
	InitRTO: 2 * time.Second, MinRTO: 1500 * time.Millisecond, MaxRTO: 8 * time.Second,
	NoPacing: true, CodelTarget: 1500 * time.Millisecond, CodelInterval: 6 * time.Second,
	AckInterval: 100 * time.Millisecond, HoleDeadline: 10 * time.Second, LaneWindow: true,
}

// The phone restarts its tunnel (a new stripe, a new random first seq)
// while the exit's stripe keeps running with pieces in flight. The new
// phone starts at the low mark of the first piece it gets, and pieces in
// flight carry a mark from before the old phone's last acks: the exit
// never sends those seqs again. The new phone used to wait on them for the
// hole deadline (10 s on cups.online) and heard nothing meanwhile.
func TestStripedSenderServesARestartedPeer(t *testing.T) {
	const lanes = 4
	var aLanes, bLanes []Transport
	for i := 0; i < lanes; i++ {
		a := &lossyLane{delay: 300 * time.Millisecond, rnd: rand.New(rand.NewSource(int64(i + 1)))}
		b := &lossyLane{delay: 300 * time.Millisecond, rnd: rand.New(rand.NewSource(int64(i + 100)))}
		a.peer, b.peer = b, a
		aLanes, bLanes = append(aLanes, a), append(bLanes, b)
	}
	exit := NewStripedTransport(aLanes)
	exit.SetOptions(cupsLikeOptions)
	var fromPhone atomic.Int32
	exit.Receive(func([]byte) { fromPhone.Add(1) })
	if err := exit.Start(); err != nil {
		t.Fatal(err)
	}
	defer exit.Stop()

	newPhone := func() (*StripedTransport, *atomic.Int32) {
		p := NewStripedTransport(bLanes)
		p.SetOptions(cupsLikeOptions)
		var got atomic.Int32
		p.Receive(func([]byte) { got.Add(1) })
		if err := p.Start(); err != nil {
			t.Fatal(err)
		}
		return p, &got
	}
	frame := func(i int) []byte {
		b := make([]byte, 1200)
		binary.BigEndian.PutUint32(b, uint32(i))
		return b
	}
	var cur *StripedTransport
	wait := func(what string, within time.Duration, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(within)
		for !ok() {
			if time.Now().After(deadline) {
				cur.ro.mu.Lock()
				next, buffered := cur.ro.next, cur.ro.count
				cur.ro.mu.Unlock()
				exit.snd.mu.Lock()
				low := exit.snd.low
				exit.snd.mu.Unlock()
				t.Fatalf("%s: not within %v; the phone waits on seq %d with %d buffered, the exit's low mark is %d",
					what, within, next, buffered, low)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	phone, got := newPhone()
	cur = phone
	// A download under way: the exit streams, the phone acks.
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			exit.Send(frame(i))
			time.Sleep(2 * time.Millisecond)
		}
	}()
	wait("the first phone hears the exit", 5*time.Second, func() bool { return got.Load() > 200 })

	// The phone restarts mid-stream: its stripe is gone with whatever was
	// in flight, a new one starts and speaks first.
	phone.Stop()
	phone2, got2 := newPhone()
	cur = phone2
	defer phone2.Stop()
	for i := 0; i < 5; i++ {
		phone2.Send(frame(i))
	}
	start := time.Now()
	wait("the new phone hears the exit", 3*time.Second, func() bool { return got2.Load() > 200 })
	stop.Store(true)
	wg.Wait()
	t.Logf("the new phone got %d frames, the first after %v", got2.Load(), time.Since(start))
}
