package main

import (
	"sync"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/ipc"
)

// The fast path of the document checks. A node tells which of its carriers
// hold their documents (IPC status Attached); a document its running node
// has not held for docOffAfter is checked at once, and again every
// fastCheckEvery while it stays off, instead of at the next round
// (docCheckEvery). A blocked document is then replaced within minutes, and
// the app learns the new link over the document that still works (the
// node's ConfigOffer), before that one fails too. A check counts only a
// blocked document (mailru.CheckDocument), so a document that is merely
// unreachable for a while is left alone.

const (
	docOffAfter    = 3 * time.Minute
	fastCheckEvery = time.Minute
	// docForget drops a document no node has reported for this long:
	// replaced, or its client gone.
	docForget = 5 * time.Minute
)

type watchedDoc struct {
	off     time.Time // since when its node has not held it; zero: held
	seen    time.Time // the last report of its node
	checked time.Time // the last fast check
}

var docWatch = struct {
	sync.Mutex
	docs map[string]*watchedDoc // by URL
}{docs: map[string]*watchedDoc{}}

// observeDocs records which of c's documents its node holds now. A node
// that started less than noDocAfter ago is still joining them, and one
// whose image does not report its carriers says nothing.
func observeDocs(c Client, st ipc.StatusPayload, up time.Duration, now time.Time) {
	if st.Carriers == 0 {
		return
	}
	held := map[string]bool{}
	for _, name := range st.Attached {
		held[name] = true
	}
	docs := c.Docs()
	names := carrierNames(docs)
	docWatch.Lock()
	defer docWatch.Unlock()
	for i, d := range docs {
		w := docWatch.docs[d.URL]
		if w == nil {
			w = &watchedDoc{}
			docWatch.docs[d.URL] = w
		}
		w.seen = now
		// A classic node names its one carrier after its type.
		on := held[names[i]] || (len(docs) == 1 && len(st.Attached) > 0)
		switch {
		case on || up < noDocAfter:
			w.off = time.Time{}
		case w.off.IsZero():
			w.off = now
		}
	}
}

// docsOff returns the documents off their node for docOffAfter and not
// checked in the last fastCheckEvery, and counts them as checked now.
func docsOff(now time.Time) []string {
	docWatch.Lock()
	defer docWatch.Unlock()
	var out []string
	for url, w := range docWatch.docs {
		switch {
		case now.Sub(w.seen) > docForget:
			delete(docWatch.docs, url)
		case !w.off.IsZero() && now.Sub(w.off) >= docOffAfter && now.Sub(w.checked) >= fastCheckEvery:
			w.checked = now
			out = append(out, url)
		}
	}
	return out
}
