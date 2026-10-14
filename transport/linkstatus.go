package transport

import (
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/ipc"
)

// LinkStatuses tells how each carrier is doing, highest priority first:
// with several documents of equal priority, which of them reach the peer
// and how the traffic spreads over them.
func (s *Session) LinkStatuses() []ipc.LinkStatus {
	now := time.Now()
	flows := s.placement.flowCounts(now)
	type state struct {
		l              *transportLink
		attached, live bool
		heard          time.Time
	}
	s.mu.Lock()
	states := make([]state, 0, len(s.order))
	for _, name := range s.order {
		l := s.links[name]
		states = append(states, state{l, s.connectedLocked(l), s.heardLocked(l), l.lastHeard})
	}
	s.mu.Unlock()

	out := make([]ipc.LinkStatus, 0, len(states))
	for _, st := range states {
		stats := st.l.raw.Stats()
		ls := ipc.LinkStatus{
			Name:     st.l.name,
			Attached: st.attached,
			Live:     st.live,
			BytesIn:  stats.BytesReceived,
			BytesOut: stats.BytesSent,
			Queued:   st.l.batched.Backlog(),
			Flows:    flows[st.l.name],
		}
		if !st.heard.IsZero() {
			ls.HeardMs = st.heard.UnixMilli()
		}
		out = append(out, ls)
	}
	return out
}
