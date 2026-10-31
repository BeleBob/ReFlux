package cupsonline

import (
	"testing"
	"time"
)

// A room whose subscription has gone one-way is asked to come back (see
// transport.LaneReviver): the cheap way hangs up and subscribes again on the
// tokens it has, the hard way enters the room first for fresh ones.
func TestCupsLaneComesBackWhenRevived(t *testing.T) {
	f := newFakeCups(t)
	_, client := startStripedPair(t, fastConfig())
	ws := client.wss[0]
	lane := &cupsLane{ws: ws}

	conns := ws.stats.reconnects.Load()
	hits := f.hits(ws.roomUUID)
	lane.ReviveLane(false)
	waitFor(t, 5*time.Second, "the room to reconnect", func() bool {
		return ws.stats.reconnects.Load() > conns
	})
	waitFor(t, 5*time.Second, "the room to be up again", func() bool {
		return ws.connected.Load()
	})
	if got := f.hits(ws.roomUUID); got != hits {
		t.Errorf("a reconnect loaded the room page %d time(s): it should reuse the tokens", got-hits)
	}

	lane.ReviveLane(true)
	waitFor(t, 5*time.Second, "the room to be entered again", func() bool {
		return f.hits(ws.roomUUID) > hits
	})
	waitFor(t, 5*time.Second, "the room to be up again after the re-entry", func() bool {
		return ws.connected.Load()
	})
}
