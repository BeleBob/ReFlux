package transport

import (
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/control"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// A carrier that works one way only. The peer counts a carrier as live
// while it hears the peer on it (heardLocked), and anything heard counts,
// pings too. A carrier that carried the phone's pings to the exit but none
// of the exit's packets to the phone (cups.online on a mobile network,
// 2026-10-20; a stuck stripe sender, 10-19) stayed live on the exit for
// good: the exit kept answering into it, the phone heard nothing, and the
// mail.ru carriers standing by were never used.
//
// Two rules take such a carrier out of what data is sent on, as long as
// another carrier still works:
//
//   - unanswered: data went out on a carrier and no answer came back on it:
//     neither data nor a pong (the peer's own pings do not count: they
//     still arrive when only our packets are lost). After
//     probeUnansweredAfter it gets a ping, which a working carrier answers
//     within a round trip; after unansweredAfter without an answer it is
//     left until it answers again. Only against a peer known to answer
//     pings (peerKeepalive).
//   - the exit follows the client's data: a client sends its data on the
//     carriers it hears the exit on, so while it sends data, the exit
//     answers on the carriers that data came in on (within peerDataWindow)
//     and not on one it only hears pings on.
//
// Neither changes the wire: both read what the peer already sends.

// answeredAt is when the peer last answered on l: data or a pong.
func (l *transportLink) answeredAt() time.Time {
	if l.pongHeard.After(l.dataHeard) {
		return l.pongHeard
	}
	return l.dataHeard
}

// sentDataLocked notes data handed to a carrier: the first packet since the
// peer last answered on it starts its wait for an answer. It reports
// whether the carrier should be pinged now. Caller holds s.mu.
func (s *Session) sentDataLocked(l *transportLink, now time.Time) (probe bool) {
	if !l.sendWait.After(l.answeredAt()) {
		l.sendWait, l.probed = now, false
		return false
	}
	if !l.probed && now.Sub(l.sendWait) >= s.probeUnansweredAfter {
		l.probed = true
		return true
	}
	return false
}

// probeUnanswered pings a carrier that has not answered our data.
func (s *Session) probeUnanswered(l *transportLink) {
	if err := s.sendControlVia(l, control.SubtypeLinkPing, nil); err != nil {
		utils.Debugf("[SESSION] LinkPing via quiet %q: %v", l.name, err)
	}
}

// unansweredLocked: data has gone out on l for unansweredAfter and nothing
// came back on it. Caller holds s.mu.
func (s *Session) unansweredLocked(l *transportLink, now time.Time) bool {
	return s.peerKeepalive && s.unansweredAfter > 0 &&
		l.sendWait.After(l.answeredAt()) && now.Sub(l.sendWait) >= s.unansweredAfter
}

// answeringLocked narrows heard, the carriers the peer was heard on, to the
// ones the rules above leave for data; it returns heard unchanged when they
// would leave none. Caller holds s.mu.
func (s *Session) answeringLocked(heard []*transportLink, now time.Time) []*transportLink {
	out := heard
	if s.exit && s.peerDataWindow > 0 {
		if f := keepLinks(out, func(l *transportLink) bool { return now.Sub(l.dataHeard) < s.peerDataWindow }); len(f) > 0 {
			out = f
		}
	}
	if f := keepLinks(out, func(l *transportLink) bool { return !s.unansweredLocked(l, now) }); len(f) > 0 {
		out = f
	}
	if len(out) < len(heard) && utils.Throttled("session.answering", time.Minute) {
		var left []string
		for _, l := range heard {
			if !containsLink(out, l) {
				left = append(left, l.name)
			}
		}
		utils.Infof("[SESSION] %v carry nothing back from the peer: sending on %v", left, linkNames(out))
	}
	return out
}

func keepLinks(links []*transportLink, keep func(*transportLink) bool) []*transportLink {
	var out []*transportLink
	for _, l := range links {
		if keep(l) {
			out = append(out, l)
		}
	}
	return out
}

func containsLink(links []*transportLink, l *transportLink) bool {
	for _, x := range links {
		if x == l {
			return true
		}
	}
	return false
}

func linkNames(links []*transportLink) []string {
	out := make([]string, len(links))
	for i, l := range links {
		out[i] = l.name
	}
	return out
}
