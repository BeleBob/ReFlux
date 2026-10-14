package main

import (
	"strconv"
	"strings"

	"github.com/p1neappleXpress/OpenFlux/transport/ipc"
)

// How each of a client's documents is doing, from its node's carrier of
// the same name (carrierNames). The node shares the client's flows among
// the documents that reach the client (docPriority), so each one is either
// carrying traffic or out of it: dead, or missing from an older link in
// the app.

// docStatus is one document as the node reports it.
type docStatus struct {
	known    bool   // the node reported on it
	attached bool   // the node holds the document
	live     bool   // the client is heard over it, so traffic goes there
	down, up uint64 // bytes the node sent and received over it since it started
	flows    int    // flows the node sends over it now
}

// docStatuses lines the node's carriers up with c's documents. A node image
// older than per-carrier status (IPC links) tells which carriers hold their
// document and, with a client there, which carry the traffic; no figures.
func docStatuses(c Client, st ipc.StatusPayload) []docStatus {
	names := carrierNames(c.Docs())
	out := make([]docStatus, len(names))
	at := make(map[string]int, len(names))
	for i, n := range names {
		at[n] = i
	}
	if len(st.Links) > 0 {
		for _, l := range st.Links {
			if i, ok := at[l.Name]; ok {
				out[i] = docStatus{known: true, attached: l.Attached, live: l.Live,
					down: l.BytesOut, up: l.BytesIn, flows: l.Flows}
			}
		}
		return out
	}
	for _, n := range st.Attached {
		if i, ok := at[n]; ok {
			out[i].known, out[i].attached = true, true
		}
	}
	if st.Connected {
		for _, n := range st.ActiveAll {
			if i, ok := at[n]; ok {
				out[i].known, out[i].live = true, true
			}
		}
	}
	return out
}

// state is a message id for how the document is doing while the client is
// online or not; "" when the node did not say.
func (d docStatus) state(online bool) string {
	switch {
	case !d.known:
		return ""
	case d.live:
		return "doc.live"
	case !d.attached:
		return "doc.detached"
	case online:
		return "doc.cold"
	default:
		return "doc.waiting"
	}
}

// docMarks are the bot's marks and the panel's pill classes for states.
var docMarks = map[string]struct{ emoji, pill string }{
	"doc.live":     {"✅", "ok"},
	"doc.cold":     {"⚠️", "warn"},
	"doc.waiting":  {"⏳", ""},
	"doc.detached": {"❌", "FAIL"},
}

// coldDocs numbers (from 1) the documents an online client is not heard
// over; ok is false when it is heard over none of them, so the node's
// carriers could not be told apart.
func coldDocs(docs []docStatus) (cold []int, ok bool) {
	for i, d := range docs {
		if d.live {
			ok = true
		} else {
			cold = append(cold, i+1)
		}
	}
	return cold, ok
}

// docNumbers lists document numbers: "2, 3".
func docNumbers(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ", ")
}
