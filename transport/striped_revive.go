package transport

import (
	"encoding/binary"
	"time"

	"github.com/p1neappleXpress/OpenFlux/utils"
)

// A lane can go one-way without saying so: its link stays up, it keeps taking
// what this side puts on it, and nothing of that ever reaches the peer. Two
// of a phone's four cups.online rooms did that for a whole session - the
// phone's per-lane figures read r0 on both while the other two carried
// ~95 KB/s, and the same rooms from another machine at the same time
// delivered on all four, so neither the rooms nor the far side were at fault.
// The stripe hides it (the pieces go again on the lanes that work) at the
// price of most of the throughput and a retransmission timeout per piece.
//
// Neither side can see this alone: the sender's lane looks fine, and the
// receiver cannot tell a lane the sender is not using from one whose pieces
// never arrive. So each side appends to its acks how many pieces it has taken
// off each lane (withLaneCounts), and the sender compares that with what it
// put there: a lane it keeps filling and the peer never hears from is
// one-way, and its carrier is asked to put the link back up - first a
// reconnect, then, if it stays dead, the expensive way.
//
// The counts ride the acks, which flow anyway, and sit behind the runs an ack
// names, where a peer from before this change never looks; a peer that never
// sends them leaves this side doing nothing, as before. Lane indices are the
// same on both ends (a cups room list is joined in the order the exit packed
// it, and a grown lane is mirrored), and a report for another number of lanes
// is ignored.

// LaneReviver is a lane whose carrier can put its link back up. hard asks for
// the expensive way (re-entering the room with fresh credentials) instead of
// a reconnect.
type LaneReviver interface {
	ReviveLane(hard bool)
}

// laneCountsMagic marks the ack trailer: one byte, then the number of lanes,
// then that many 4-byte counts, big-endian.
const laneCountsMagic = 0xAC

var (
	// laneQuietCheck is how often the lanes are looked at.
	laneQuietCheck = 5 * time.Second
	// laneQuietFor is how long a lane this side keeps filling may bring the
	// peer nothing.
	laneQuietFor = 45 * time.Second
	// laneQuietCooldown is the least time between two revivals of one lane:
	// re-entering a room costs page loads, and a storm of those is how an IP
	// gets rate-limited.
	laneQuietCooldown = 3 * time.Minute
	// laneQuietBusy is how many pieces must have gone onto a lane between two
	// checks for the peer hearing none of them to mean anything.
	laneQuietBusy = uint64(8)
)

// withLaneCounts appends this side's per-lane piece counts to an ack.
func (s *StripedTransport) withLaneCounts(ack []byte) []byte {
	lanes, _ := s.snapshotLanes()
	n := min(len(lanes), maxStripeLanes)
	if n < 2 || len(ack) < ackHeader {
		return ack
	}
	ack = append(ack, laneCountsMagic, byte(n))
	for i := 0; i < n; i++ {
		ack = binary.BigEndian.AppendUint32(ack, uint32(s.laneRecv[i].Load()))
	}
	return ack
}

// takeLaneCounts keeps the counts an ack carries, if it carries any.
func (s *StripedTransport) takeLaneCounts(ack []byte) {
	at := ackHeader + 6*int(ack[9])
	if len(ack) < at+2 || ack[at] != laneCountsMagic {
		return
	}
	n := int(ack[at+1])
	if n < 2 || n > maxStripeLanes || len(ack) < at+2+4*n {
		return
	}
	got := make([]uint32, n)
	for i := range got {
		got[i] = binary.BigEndian.Uint32(ack[at+2+4*i:])
	}
	s.peerGotMu.Lock()
	s.peerGot = got
	s.peerGotMu.Unlock()
}

// peerGotCounts is the far side's last counts, or nil when it has sent none
// or counts a different number of lanes.
func (s *StripedTransport) peerGotCounts(lanes int) []uint32 {
	s.peerGotMu.Lock()
	defer s.peerGotMu.Unlock()
	if len(s.peerGot) != lanes {
		return nil
	}
	return append([]uint32(nil), s.peerGot...)
}

// piecesPerLane is how many pieces have left on each lane, retransmissions
// included: what the far side should have taken off it.
func (q *sender) piecesPerLane(n int) []uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]uint64, n)
	for i := 0; i < n && i < maxStripeLanes; i++ {
		out[i] = q.laneTx[i]
	}
	return out
}

// reviveSettings is read where the stripe is started, not in the loop, so a
// test that shortens the waits never races it.
type reviveSettings struct {
	every, silence, cooldown time.Duration
	busy                     uint64
}

func newReviveSettings() reviveSettings {
	return reviveSettings{laneQuietCheck, laneQuietFor, laneQuietCooldown, laneQuietBusy}
}

// reviveLoop runs while the stripe does.
func (s *StripedTransport) reviveLoop(cfg reviveSettings) {
	if lanes, _ := s.snapshotLanes(); len(lanes) < 2 {
		return // one lane carries everything or nothing; there is nothing to revive
	}
	var (
		mine   [maxStripeLanes]uint64    // pieces this side has put on each lane
		theirs [maxStripeLanes]uint32    // pieces the peer says it took off it
		quiet  [maxStripeLanes]int       // checks in a row: filled here, nothing there
		kicks  [maxStripeLanes]int       // revivals since the lane last carried
		kicked [maxStripeLanes]time.Time // when the last one was
	)
	need := max(int(cfg.silence/cfg.every), 1)
	tick := time.NewTicker(cfg.every)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
		}
		// The stripe may have grown a lane since the last check.
		lanes, _ := s.snapshotLanes()
		n := min(len(lanes), maxStripeLanes)
		sent := s.snd.piecesPerLane(n)
		peer := s.peerGotCounts(n)
		now := time.Now()
		for i := 0; i < n; i++ {
			put := sent[i] - mine[i]
			mine[i] = sent[i]
			if peer == nil {
				quiet[i] = 0 // a peer that says nothing: as before this change
				continue
			}
			arrived := peer[i] - theirs[i] // wraps with the counts
			theirs[i] = peer[i]
			rev, ok := lanes[i].(LaneReviver)
			if !ok {
				continue
			}
			if arrived > 0 {
				quiet[i], kicks[i] = 0, 0
				continue
			}
			if put < cfg.busy || !lanes[i].IsConnected() {
				quiet[i] = 0 // nothing went there, or its carrier knows it is down
				continue
			}
			quiet[i]++
			if quiet[i] < need || now.Sub(kicked[i]) < cfg.cooldown {
				continue
			}
			hard := kicks[i] > 0
			if hard {
				utils.Infof("[STRIPE] lane %d still brings the peer nothing of what goes on it: re-entering it", i)
			} else {
				utils.Infof("[STRIPE] lane %d has brought the peer nothing for %s while this side kept filling it: reconnecting it", i, cfg.silence)
			}
			rev.ReviveLane(hard)
			quiet[i], kicks[i], kicked[i] = 0, kicks[i]+1, now
		}
	}
}
