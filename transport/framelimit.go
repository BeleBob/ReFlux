package transport

import "time"

// FrameLimiter is a carrier whose service refuses frames above a size: since
// 2026-10-12 Mail.ru's document server answers a cursor message carrying more
// than about 3 KB with "access deny" and drops the connection. The batching
// layers above such a carrier keep every frame under the limit.
type FrameLimiter interface {
	// MaxFrame is the largest frame, in bytes, the carrier passes on.
	MaxFrame() int
}

// frameMargin is what a frame gains on its way down from a batch: the batch
// header and AES-GCM's header, nonce and tag (33 bytes), with room to spare.
const frameMargin = 64

// batchBudget is the batch size that keeps raw's frames under its limit, or 0
// when raw has none.
func batchBudget(raw Transport) int {
	if fl, ok := raw.(FrameLimiter); ok && fl.MaxFrame() > frameMargin {
		return fl.MaxFrame() - frameMargin
	}
	return 0
}

// fillBatch collects a batch after first: what is queued already, then what
// arrives within lingerMs. It stops at maxBytes or maxCount, and before a
// packet that would take the batch past limit (0: no limit), which comes back
// as carry to start the next batch. stopped reports a close of stop while
// lingering; the batch is then dropped, as before.
func fillBatch(queue <-chan []byte, stop <-chan struct{}, first []byte, maxBytes, maxCount, lingerMs, limit int) (batch [][]byte, size int, carry []byte, stopped bool) {
	batch = [][]byte{first}
	size = 2 + len(first)
	add := func(p []byte) bool {
		if limit > 0 && size+2+len(p) > limit {
			carry = p
			return false
		}
		batch = append(batch, p)
		size += 2 + len(p)
		return true
	}
	full := func() bool { return carry != nil || size >= maxBytes || len(batch) >= maxCount }

drain:
	for !full() {
		select {
		case p := <-queue:
			if !add(p) {
				break drain
			}
		default:
			break drain
		}
	}
	if lingerMs > 0 && !full() {
		timer := time.NewTimer(time.Duration(lingerMs) * time.Millisecond)
		defer timer.Stop()
	linger:
		for !full() {
			select {
			case <-stop:
				return batch, size, carry, true
			case p := <-queue:
				if !add(p) {
					break linger
				}
			case <-timer.C:
				break linger
			}
		}
	}
	return batch, size, carry, false
}
