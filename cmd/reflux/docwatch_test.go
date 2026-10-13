package main

import (
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/ipc"
)

func resetDocWatch(t *testing.T) {
	docWatch.Lock()
	docWatch.docs = map[string]*watchedDoc{}
	docWatch.Unlock()
	t.Cleanup(func() {
		docWatch.Lock()
		docWatch.docs = map[string]*watchedDoc{}
		docWatch.Unlock()
	})
}

// A document its running node has not held for 3 minutes is due for a
// check, then once a minute while it stays off; held again, it is not.
func TestDocsOffTheirNodeAreCheckedAtOnce(t *testing.T) {
	resetDocWatch(t)
	c := Client{Name: "phone", Transport: "mailru", URL: testURL, Backups: []Doc{{"mailru", docB}}}
	backupOff := ipc.StatusPayload{Carriers: 2, Attached: []string{"mailru"}}
	up := 10 * time.Minute
	t0 := time.Now()
	at := func(d time.Duration) time.Time { return t0.Add(d) }

	for d := time.Duration(0); d <= 2*time.Minute; d += time.Minute {
		observeDocs(c, backupOff, up+d, at(d))
		if got := docsOff(at(d)); len(got) != 0 {
			t.Fatalf("after %v: %v due already", d, got)
		}
	}
	observeDocs(c, backupOff, up+3*time.Minute, at(3*time.Minute))
	if got := docsOff(at(3 * time.Minute)); len(got) != 1 || got[0] != docB {
		t.Fatalf("after 3 min: %v, want the backup", got)
	}
	if got := docsOff(at(3*time.Minute + 30*time.Second)); len(got) != 0 {
		t.Fatalf("checked again within a minute: %v", got)
	}
	if got := docsOff(at(4 * time.Minute)); len(got) != 1 {
		t.Fatalf("a minute later: %v, want it again", got)
	}

	// Held again: nothing is due.
	observeDocs(c, ipc.StatusPayload{Carriers: 2, Attached: []string{"mailru", "mailru-2"}}, up+5*time.Minute, at(5*time.Minute))
	if got := docsOff(at(9 * time.Minute)); len(got) != 0 {
		t.Fatalf("held again, still due: %v", got)
	}
}

// A node that just started is still joining its documents, an image that
// does not report its carriers says nothing, and a classic node names its
// one carrier as it likes.
func TestDocsOffIgnoresWhatSaysNothing(t *testing.T) {
	resetDocWatch(t)
	t0 := time.Now()
	c := Client{Name: "phone", Transport: "mailru", URL: testURL, Backups: []Doc{{"mailru", docB}}}
	none := ipc.StatusPayload{Carriers: 2}
	for d := time.Duration(0); d <= 10*time.Minute; d += time.Minute {
		observeDocs(c, none, d/4, t0.Add(d)) // restarted again and again
		observeDocs(c, ipc.StatusPayload{}, time.Hour, t0.Add(d))
	}
	if got := docsOff(t0.Add(10 * time.Minute)); len(got) != 0 {
		t.Fatalf("due: %v", got)
	}

	single := Client{Name: "tablet", Transport: "mailru", URL: docC}
	for d := time.Duration(0); d <= 10*time.Minute; d += time.Minute {
		observeDocs(single, ipc.StatusPayload{Carriers: 1, Attached: []string{"primary"}}, time.Hour, t0.Add(d))
	}
	if got := docsOff(t0.Add(10 * time.Minute)); len(got) != 0 {
		t.Fatalf("a held classic document due: %v", got)
	}

	// Not reported for a while (replaced, the client gone): forgotten.
	observeDocs(single, ipc.StatusPayload{Carriers: 1}, time.Hour, t0.Add(10*time.Minute))
	docsOff(t0.Add(11 * time.Minute))
	if got := docsOff(t0.Add(30 * time.Minute)); len(got) != 0 {
		t.Fatalf("a forgotten document due: %v", got)
	}
	docWatch.Lock()
	n := len(docWatch.docs)
	docWatch.Unlock()
	if n != 0 {
		t.Errorf("%d documents still watched", n)
	}
}

// The fast check counts like a round: two blocked answers in a row and the
// client gets a free pool document. It checks only the lost document and
// leaves the round's schedule alone.
func TestFastCheckReplacesABlockedDocument(t *testing.T) {
	s := Store{Root: t.TempDir()}
	writeFile(t, s.poolPath(), "mailru "+docB+"\nmailru "+docC+"\n")
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDoc("phone", Doc{"mailru", docB}); err != nil {
		t.Fatal(err)
	}
	fakeEgressCheck(t, docB, docC) // docC would be dead too, if it were checked
	now := time.Now()
	if changes, changed, err := s.checkClientDocs(now, []string{docB}); err != nil || changed || len(changes) != 0 {
		t.Fatalf("first answer: %+v %v %v", changes, changed, err)
	}
	changes, changed, err := s.checkClientDocs(now.Add(time.Minute), []string{docB})
	if err != nil || !changed || len(changes) != 1 || changes[0].Doc.URL != docB || changes[0].New == nil || changes[0].New.URL != docC {
		t.Fatalf("second answer: %+v %v %v", changes, changed, err)
	}
	st, _ := s.loadDocsState()
	if !st.Checked.IsZero() {
		t.Errorf("the round's schedule moved: %v", st.Checked)
	}
	if st.Docs[docC] != nil {
		t.Errorf("a pool document was checked: %+v", st.Docs[docC])
	}
	if c, _ := s.Get("phone"); len(c.Docs()) != 2 || c.Docs()[0].URL != testURL || c.Docs()[1].URL != docC {
		t.Errorf("phone's documents: %+v", c.Docs())
	}
	if changes, _, err := s.checkClientDocs(now, []string{"https://cloud.mail.ru/public/none/x"}); err != nil || len(changes) != 0 {
		t.Errorf("a document of nobody: %+v %v", changes, err)
	}
}
