package cupsonline

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// useBudgetClock runs the shared budget on a clock the test moves, and leaves
// a clean budget behind for the next test.
func useBudgetClock(t *testing.T) *time.Time {
	t.Helper()
	now := time.Unix(1_800_000_000, 0)
	old := budgetNow
	budgetNow = func() time.Time { return now }
	resetBudget(t)
	t.Cleanup(func() { budgetNow = old })
	return &now
}

func resetBudget(t *testing.T) {
	t.Helper()
	msgBudget.mu.Lock()
	msgBudget.tokens, msgBudget.last, msgBudget.paused = 0, time.Time{}, time.Time{}
	msgBudget.mu.Unlock()
	t.Cleanup(func() {
		msgBudget.mu.Lock()
		msgBudget.tokens, msgBudget.last, msgBudget.paused = 0, time.Time{}, time.Time{}
		msgBudget.mu.Unlock()
	})
}

func TestBudgetRateAndBurst(t *testing.T) {
	now := useBudgetClock(t)
	for i := 0; i < 3; i++ {
		if d := msgBudget.reserve(10, 3); d != 0 {
			t.Fatalf("message %d of the burst waits %v", i, d)
		}
	}
	if d := msgBudget.reserve(10, 3); d != 100*time.Millisecond {
		t.Fatalf("past the burst: wait %v, want 100ms", d)
	}
	*now = now.Add(100 * time.Millisecond)
	if d := msgBudget.reserve(10, 3); d != 0 {
		t.Fatalf("after 100ms at 10/s: wait %v", d)
	}
	// A long quiet spell saves up only the burst.
	*now = now.Add(time.Hour)
	for i := 0; i < 3; i++ {
		msgBudget.reserve(10, 3)
	}
	if d := msgBudget.reserve(10, 3); d == 0 {
		t.Fatal("a quiet hour saved up more than the burst")
	}
	if d := msgBudget.reserve(0, 0); d != 0 {
		t.Fatalf("rate 0 should not limit, waits %v", d)
	}
}

func TestBudgetPause(t *testing.T) {
	now := useBudgetClock(t)
	if !msgBudget.pause(5 * time.Second) {
		t.Fatal("the first 429 should start a new run")
	}
	*now = now.Add(time.Second)
	if msgBudget.pause(5 * time.Second) {
		t.Fatal("a 429 during the pause is the same run")
	}
	// The pause now ends 6 s from the start; the limit off holds too.
	if d := msgBudget.reserve(0, 0); d != 5*time.Second {
		t.Fatalf("during the pause: wait %v, want 5s", d)
	}
	*now = now.Add(5 * time.Second)
	// After it one message, not a burst saved up during the pause.
	if d := msgBudget.reserve(10, 20); d != 100*time.Millisecond {
		t.Fatalf("right after the pause: wait %v, want 100ms", d)
	}
	*now = now.Add(100 * time.Millisecond)
	if d := msgBudget.reserve(10, 20); d != 0 {
		t.Fatalf("100ms after the pause: wait %v", d)
	}
	*now = now.Add(time.Minute)
	if !msgBudget.pause(5 * time.Second) {
		t.Fatal("a 429 a minute later is a new run")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if msgBudget.take(ctx, 10, 20) {
		t.Fatal("take during a pause returned with ctx done")
	}
}

// The rooms send under a per-address limit like cups.online's and never run
// into it: the stripe's data waits for the budget instead of being refused.
func TestStripedStaysUnderAddressLimit(t *testing.T) {
	f := newFakeCups(t)
	resetBudget(t)
	f.locked(func() { f.rateLimit, f.rateWindow = 40, 200*time.Millisecond })
	cfg := fastConfig()
	cfg.MsgRate, cfg.MsgBurst = 100, 10
	exit, client := startStripedPair(t, cfg)

	const n = 100
	var got atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		expectInOrderCounting(t, client, n, 20*time.Second, &got)
	}()
	sendWindowed(t, exit, n, &got, done)
	<-done
	var refused int
	f.locked(func() { refused = f.refused })
	if refused > 0 {
		t.Fatalf("the server refused %d messages as over its limit", refused)
	}
}

// Once the server answers 429, every room holds its messages for the pause
// instead of losing one after another to the limit. The unstriped wire has no
// feedback of its own, so without the pause it keeps sending into the limit.
func TestRateLimitedRoomsPause(t *testing.T) {
	f := newFakeCups(t)
	resetBudget(t)
	f.locked(func() { f.rateLimit, f.rateWindow = 20, time.Second })
	cfg := fastConfig()
	cfg.RateLimitPause = 300 * time.Millisecond
	exit, _ := startPair(t, cfg)

	stop := time.Now().Add(2 * time.Second)
	for i := 0; time.Now().Before(stop); i++ {
		exit.Send(seqFrame(i % 50))
		time.Sleep(2 * time.Millisecond)
	}
	var refused int
	f.locked(func() { refused = f.refused })
	if refused == 0 {
		t.Fatal("the load never reached the limit; the test proves nothing")
	}
	// Measured: ~50 with the pause, well over 1000 without.
	if refused > 200 {
		t.Fatalf("the server refused %d messages: the rooms kept sending into the limit", refused)
	}
}

// A stripe lane takes from the same budget: while it is paused nothing leaves.
func TestStripedLanesHoldDuringPause(t *testing.T) {
	newFakeCups(t)
	resetBudget(t)
	exit, client := startStripedPair(t, fastConfig())

	const hold = 400 * time.Millisecond
	msgBudget.pause(hold)
	start := time.Now()
	exit.Send(seqFrame(0))
	select {
	case <-client.got:
		if waited := time.Since(start); waited < hold-50*time.Millisecond {
			t.Fatalf("a frame arrived %v into a %v pause", waited, hold)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the frame never arrived after the pause")
	}
}
