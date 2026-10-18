package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// roomsMaker makes room lists for fakeEgressCheck, numbered, until fail.
func roomsMaker(t *testing.T) (made *int, fail *error) {
	var n int
	var err error
	fakeRooms = func(count int) (string, error) {
		if err != nil {
			return "", err
		}
		n++
		ids := make([]string, count)
		for i := range ids {
			ids[i] = fmt.Sprintf("%08d-0000-4000-8000-%012d", n, i)
		}
		return transport.PackRooms(ids, true), nil
	}
	t.Cleanup(func() { fakeRooms = nil })
	return &n, &err
}

// An active client gets rooms at the check: they take the flows, its
// documents stand by, and the context stays its document.
func TestClientGetsRooms(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	old, err := s.Add("old", "mailru", docB)
	if err != nil {
		t.Fatal(err)
	}
	old.Paused = true
	if err := s.Save(old); err != nil {
		t.Fatal(err)
	}
	fakeEgressCheck(t)
	made, _ := roomsMaker(t)

	changes, changed, err := s.checkDocs(time.Now())
	if err != nil || !changed || len(changes) != 1 || !changes[0].Cups || changes[0].Client != "phone" || changes[0].New == nil {
		t.Fatalf("changes %+v %v %v", changes, changed, err)
	}
	if id, args := docChangeMessage(changes[0]); id != "docs.cups.added" || args[1] != cupsRooms {
		t.Errorf("message %s %v", id, args)
	}
	c, _ := s.Get("phone")
	docs := c.Docs()
	if len(docs) != 2 || !isCups(docs[1]) || roomCount(docs[1].URL) != cupsRooms || c.context() != testURL {
		t.Fatalf("client %+v", c)
	}
	if docPriority(docs, 0) != docsPriority || docPriority(docs, 1) != cupsPriority {
		t.Errorf("priorities %d %d", docPriority(docs, 0), docPriority(docs, 1))
	}
	conf := nodeConf(c)
	for _, want := range []string{"[Transport \"mailru\"]\nType = mailru\nPriority = 50\n", "[Transport \"cupsonline\"]\nType = cupsonline\nPriority = 100\nURL = " + docs[1].URL + "\n", "URL = " + testURL + "\n"} {
		if !strings.Contains(conf, want) {
			t.Errorf("node.conf lacks %q:\n%s", want, conf)
		}
	}
	cfg := shareConfig(c, "0123456789abcdef0123")
	if !cfg.Negotiate || cfg.Context != testURL || len(cfg.Transports) != 2 || cfg.Transports[1].Type != cupsType || cfg.Transports[1].Priority != cupsPriority {
		t.Errorf("link %+v", cfg)
	}
	// One client per check, and none twice.
	if changes, _, _ := s.checkDocs(time.Now()); len(changes) != 0 || *made != 1 {
		t.Errorf("second check: %+v, %d lists made", changes, *made)
	}
	if o, _ := s.Get("old"); o.cupsIndex() >= 0 {
		t.Errorf("a paused client got rooms: %+v", o)
	}
}

// A list with a room gone gets new rooms in its place after two checks,
// never a pool document, and goes to no dead list.
func TestGoneRoomsAreReplaced(t *testing.T) {
	s := Store{Root: t.TempDir()}
	writeFile(t, s.poolPath(), "mailru "+docC+"\n")
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDoc("phone", Doc{"mailru", docB}); err != nil {
		t.Fatal(err)
	}
	roomsMaker(t)
	fakeEgressCheck(t)
	now := time.Now()
	s.checkDocs(now)
	c, _ := s.Get("phone")
	old := c.Docs()[c.cupsIndex()]

	fakeEgressCheck(t, old.URL)
	if changes, _, _ := s.checkDocs(now); len(changes) != 0 {
		t.Fatalf("one bad answer: %+v", changes)
	}
	changes, changed, err := s.checkDocs(now)
	if err != nil || !changed || len(changes) != 1 {
		t.Fatalf("changes %+v %v %v", changes, changed, err)
	}
	ch := changes[0]
	if !ch.Cups || ch.Doc.URL != old.URL || ch.New == nil || ch.Err != nil {
		t.Errorf("change %+v", ch)
	}
	if id, _ := docChangeMessage(ch); id != "docs.cups.replaced" {
		t.Errorf("message %s", id)
	}
	c, _ = s.Get("phone")
	docs := c.Docs()
	if len(docs) != 3 || docs[2].URL != ch.New.URL || docs[2].URL == old.URL || docs[1].URL != docB {
		t.Errorf("documents after: %+v", docs)
	}
	st, _ := s.loadDocsState()
	if len(st.Docs) != 0 || st.Suspect[old.URL] != 0 {
		t.Errorf("the gone rooms in the state: %+v", st)
	}
	if d, err := s.freeDoc(c); err != nil || d.URL != docC {
		t.Errorf("the pool document was taken: %+v %v", d, err)
	}
}

// When cups.online makes no rooms, the gone list goes and the channel
// stays on mail.ru; the next tries wait cupsRetry, the doctor says why.
func TestNoRoomsFromCups(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	made, fail := roomsMaker(t)
	calls := fakeEgressCheck(t)
	now := time.Now()
	s.checkDocs(now)
	c, _ := s.Get("phone")
	old := c.Docs()[c.cupsIndex()]

	*fail = errors.New("could not create any room: GET room status 429")
	fakeEgressCheck(t, old.URL)
	s.checkDocs(now)
	changes, changed, err := s.checkDocs(now)
	if err != nil || !changed || len(changes) != 1 || changes[0].New != nil || changes[0].Err == nil {
		t.Fatalf("changes %+v %v %v", changes, changed, err)
	}
	if id, _ := docChangeMessage(changes[0]); id != "docs.cups.dropped" {
		t.Errorf("message %s", id)
	}
	if c, _ := s.Get("phone"); len(c.Docs()) != 1 || c.URL != testURL || c.NoCups {
		t.Errorf("client after: %+v", c)
	}
	// Within the hour nobody asks again: neither that check, for the
	// client now without rooms, nor the later ones.
	calls = fakeEgressCheck(t)
	if changes, _, _ := s.checkDocs(now.Add(30 * time.Minute)); len(changes) != 0 {
		t.Errorf("within the hour: %+v", changes)
	}
	for _, c := range *calls {
		if strings.Contains(c, "create-rooms") {
			t.Errorf("rooms asked for within the hour: %v", *calls)
		}
	}
	d := &doctor{}
	d.cups(s)
	if len(d.findings) != 1 || d.findings[0].Msg != "cups.failing" || d.findings[0].Level != levelWarn {
		t.Errorf("doctor %+v", d.findings)
	}
	*fail = nil
	changes, _, _ = s.checkDocs(now.Add(cupsRetry + time.Minute))
	if len(changes) != 1 || !changes[0].Cups || changes[0].New == nil || *made != 2 {
		t.Errorf("after the hour: %+v (%d made)", changes, *made)
	}
	d = &doctor{}
	if d.cups(s); len(d.findings) != 0 {
		t.Errorf("doctor after rooms were made: %+v", d.findings)
	}
}

// Rooms the owner takes away stay away and never go to the pool; cups
// add gives them back. With rooms off, nobody gets new ones.
func TestOwnerTakesRoomsAway(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	roomsMaker(t)
	fakeEgressCheck(t)
	now := time.Now()
	s.checkDocs(now)
	c, _ := s.Get("phone")
	rooms := c.Docs()[c.cupsIndex()]
	if c, err := s.TakeDoc("phone", c.cupsIndex()); err != nil || !c.NoCups || len(c.Docs()) != 1 {
		t.Fatalf("take: %+v %v", c, err)
	}
	if docs, _ := s.pool(); len(docs) != 0 {
		t.Errorf("rooms in the pool: %+v", docs)
	}
	if st, _ := s.loadDocsState(); st.Docs[rooms.URL] != nil {
		t.Errorf("rooms in quarantine: %+v", st.Docs)
	}
	if changes, _, _ := s.checkDocs(now); len(changes) != 0 {
		t.Errorf("rooms came back on their own: %+v", changes)
	}
	if c, err := cupsNow(s, "phone", now); err != nil || c.NoCups || c.cupsIndex() < 0 {
		t.Errorf("cups add: %+v %v", c, err)
	}

	if err := s.setCupsOff(true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("tablet", "mailru", docB); err != nil {
		t.Fatal(err)
	}
	if changes, _, _ := s.checkDocs(now); len(changes) != 0 {
		t.Errorf("rooms while off: %+v", changes)
	}
}

// Backups are counted without the rooms: a client with rooms still gets
// its documents up to docsWanted.
func TestBackupsBesideRooms(t *testing.T) {
	s := Store{Root: t.TempDir()}
	writeFile(t, s.poolPath(), "mailru "+docB+"\n")
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	roomsMaker(t)
	fakeEgressCheck(t)
	if _, err := cupsNow(s, "phone", time.Now()); err != nil {
		t.Fatal(err)
	}
	changes, _, err := s.checkDocs(time.Now())
	if err != nil || len(changes) != 1 || !changes[0].Backup || changes[0].Doc.URL != docB {
		t.Fatalf("changes %+v %v", changes, err)
	}
	c, _ := s.Get("phone")
	if names := carrierNames(c.Docs()); strings.Join(names, ",") != "mailru,cupsonline,mailru-2" {
		t.Errorf("carriers %v", names)
	}
}
