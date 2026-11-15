package transport

import (
	"time"

	"github.com/p1neappleXpress/OpenFlux/utils"
)

// A lane's carrier may lose its place for good while the stripe runs: a
// Telemost participant whose WebSocket dropped is refused when it says hello
// again (close 4000 "TIE-0003"), every time. Only a new participant gets in,
// and that is a new lane object. SwapLane puts it where the old one was: the
// lane keeps its index - the far side pairs lanes by index - and the old one
// is stopped.

// SwapLane makes l lane idx in place of the lane there now, and stops that
// one. l should already be started. It is ignored (and l stopped) once the
// stripe has stopped or when there is no lane idx.
func (s *StripedTransport) SwapLane(idx int, l Transport) {
	select {
	case <-s.done:
		_ = l.Stop()
		return
	default:
	}
	s.lanesMu.Lock()
	if idx < 0 || idx >= len(s.lanes) {
		s.lanesMu.Unlock()
		_ = l.Stop()
		return
	}
	old := s.lanes[idx]
	// Readers keep the slices they took (snapshotLanes): copy, never write
	// into them.
	lanes := append([]Transport(nil), s.lanes...)
	readyAt := append([]time.Time(nil), s.readyAt...)
	lanes[idx] = l
	// The far side must find the new participant and subscribe to it first.
	readyAt[idx] = time.Now().Add(laneWarmup)
	s.lanes, s.readyAt = lanes, readyAt
	s.lanesMu.Unlock()

	if w, ok := old.(LaneWaker); ok {
		s.pumpMu.Lock()
		for i, x := range s.wakers {
			if x == w {
				s.wakers = append(s.wakers[:i:i], s.wakers[i+1:]...)
				break
			}
		}
		s.pumpMu.Unlock()
	}
	s.attach(idx, l)
	utils.Infof("[STRIPE] lane %d replaced", idx)
	go func() { _ = old.Stop() }()
}

// laneAt is lane idx now, nil if there is none.
func (s *StripedTransport) laneAt(idx int) Transport {
	s.lanesMu.RLock()
	defer s.lanesMu.RUnlock()
	if idx < 0 || idx >= len(s.lanes) {
		return nil
	}
	return s.lanes[idx]
}
