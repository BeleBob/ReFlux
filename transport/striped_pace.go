package transport

import "time"

// A carrier over a stripe may pace the batching layer above it (Pacer): the
// stripe's own send queue is a FIFO of frames that already mix every flow, so
// a download that fills it makes every other flow wait as long - seconds on
// cups.online, whose rooms carry ~70 KB/s. Paced, the batching layer closes a
// batch only once the stripe's queue is nearly empty, and the backlog waits
// above, where it is kept short and ordered by flow (pacing).

// stripePaceBytes is how much the stripe's send queue may hold before the
// layer above waits: a few messages' worth, so the lanes always have data
// for their next message.
const stripePaceBytes = 24 << 10

// WaitSend blocks until the stripe is connected and its send queue holds
// less than stripePaceBytes; false once stop is closed or the stripe stops.
func (s *StripedTransport) WaitSend(stop <-chan struct{}) bool {
	for {
		if s.IsConnected() && s.snd.queued() < stripePaceBytes {
			return true
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-stop:
			timer.Stop()
			return false
		case <-s.done:
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// queued is how many bytes wait in the send queue.
func (q *sender) queued() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.queueBytes
}
