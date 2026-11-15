package yandex

import (
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// A lane whose WebSocket drops cannot come back as the participant it was:
// the SFU answers its hello with close 4000 "TIE-0003", every time, so the
// lane redialled forever and the stripe went down with it once every lane
// had dropped (a mobile network that blinks, or the SFU ending the session).
// The room takes a new participant, though. So a lane of a group that can
// swap lanes (SetSwapHook) does not redial: the group joins a new lane in
// its place - the same tag, so the far side pairs it with the same lane of
// its own - and the stripe stops the old one.

// SetSwapHook gives the group the stripe's SwapLane.
func (g *TelemostGroup) SetSwapHook(f func(idx int, l transport.Transport)) {
	g.mu.Lock()
	g.swap = f
	g.mu.Unlock()
}

// rejoin replaces lane old with a new participant, in the background, after
// the usual reconnect backoff; false when the group cannot swap lanes and old
// is to reconnect itself.
func (g *TelemostGroup) rejoin(old *TelemostTransport) bool {
	g.mu.Lock()
	swap := g.swap
	g.mu.Unlock()
	if swap == nil {
		return false
	}
	if !old.IsRunning() {
		return true // stopped, or replaced already
	}
	idx := old.lane.index
	go func() {
		for attempt := 1; ; attempt++ {
			select {
			case <-time.After(reconnectBackoff(attempt)):
			case <-old.Done():
				return
			}
			nl := g.laneAt(idx)
			if err := nl.Start(); err != nil {
				utils.Debugf("[Telemost] lane %d rejoin #%d: %v", idx, attempt, err)
				_ = nl.Stop()
				continue
			}
			g.mu.Lock()
			current := idx < len(g.lanes) && g.lanes[idx] == old
			if current {
				g.lanes[idx] = nl
			}
			g.mu.Unlock()
			if !current {
				_ = nl.Stop()
				return
			}
			utils.Infof("[Telemost] lane %d lost its place in the room: joined again as a new participant", idx)
			swap(idx, nl)
			return
		}
	}()
	return true
}
