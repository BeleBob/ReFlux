package cupsonline

import (
	"context"
	"sync"
	"time"
)

// cups.online counts the cursor RPCs coming from one IP address, whatever
// session, user or room they are for, and past its limit answers every one
// with 429 ("Превышено допустимое число отправок запроса") for half a minute
// or so while the sockets stay up. Measured 2026-11: 12 messages/s held for
// minutes, 25/s ran into it after a minute, ~40/s after 26 s. So every room of
// every cups transport in the process sends out of one budget, and a 429 stops
// them all for a while instead of losing message after message to it.

// sendBudget is a token bucket shared by all the rooms of a process, with a
// pause on top that a 429 starts.
type sendBudget struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	paused time.Time // nothing leaves before this
}

// msgBudget is the process's budget: the limit is per address, not per room.
var msgBudget sendBudget

// budgetNow is the budget's clock; tests move it.
var budgetNow = time.Now

// take waits until one message may leave at rate messages a second with at
// most burst saved up, or until a pause is over. rate <= 0 lets messages go
// at once (pauses still hold them). False when ctx ends first.
func (b *sendBudget) take(ctx context.Context, rate float64, burst int) bool {
	for {
		wait := b.reserve(rate, burst)
		if wait <= 0 {
			return true
		}
		if !sleepCtx(ctx, wait) {
			return false
		}
	}
}

// reserve takes a token if there is one (and returns 0), or says how long
// until there may be.
func (b *sendBudget) reserve(rate float64, burst int) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := budgetNow()
	if now.Before(b.paused) {
		return b.paused.Sub(now)
	}
	if rate <= 0 {
		return 0
	}
	if b.last.IsZero() {
		b.tokens = float64(max(burst, 1))
	} else {
		b.tokens = min(float64(max(burst, 1)), b.tokens+now.Sub(b.last).Seconds()*rate)
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return 0
	}
	return time.Duration((1 - b.tokens) / rate * float64(time.Second))
}

// pause holds every message back for d and empties the bucket, so what leaves
// after it is one message to test the water, not a burst. It says whether
// this starts a new run of refusals (the last pause ended a while ago), the
// one worth logging.
func (b *sendBudget) pause(d time.Duration) (fresh bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := budgetNow()
	fresh = now.After(b.paused.Add(d))
	if until := now.Add(d); until.After(b.paused) {
		b.paused = until
	}
	b.tokens = 0
	b.last = b.paused // the bucket fills from the end of the pause
	return fresh
}
