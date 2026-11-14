package cupsonline

// The batching layer above a striped cups transport waits for the stripe
// (transport.Pacer), so a download no longer queues seconds of frames in it
// ahead of every other flow; see transport/striped_pace.go.

// cupsQueueBytes is how much bulk may wait above the stripe: about two
// seconds of what the rooms carry under cups.online's message limit
// (~70 KB/s), one second of it for one flow.
const cupsQueueBytes = 128 << 10

// WaitSend is transport.Pacer: on the striped wire it waits for the
// stripe's send queue to run nearly empty; the old wire does not wait.
func (t *CupsonlineTransport) WaitSend(stop <-chan struct{}) bool {
	if st := t.stripe.Load(); st != nil {
		return st.WaitSend(stop)
	}
	return true
}

// QueueBytes is transport.Pacer.
func (t *CupsonlineTransport) QueueBytes() int { return cupsQueueBytes }
