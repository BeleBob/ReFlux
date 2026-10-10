package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeEgressCheck answers check-docs: the links in dead are dead, the rest
// open. It records whether a link ever went into the arguments.
func fakeEgressCheck(t *testing.T, dead ...string) *[]string {
	var calls []string
	old := runDockerIn
	runDockerIn = func(stdin io.Reader, stdout io.Writer, args ...string) error {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		if strings.Contains(call, "http") {
			t.Errorf("a document link in the docker arguments: %s", call)
		}
		if call != "exec -i reflux-egress reflux-egress check-docs" {
			return fmt.Errorf("unexpected docker %s", call)
		}
		b, _ := io.ReadAll(stdin)
		for i, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			v := map[string]any{"line": i + 1, "state": "ok"}
			for _, d := range dead {
				if strings.HasSuffix(line, " "+d) {
					v["state"], v["error"] = "dead", "API returned status 404"
				}
			}
			json.NewEncoder(stdout).Encode(v)
		}
		return nil
	}
	t.Cleanup(func() { runDockerIn = old })
	return &calls
}

// A client's document that stops opening is replaced from the pool after
// two checks in a row, not after one, and goes to the dead list.
func TestDeadDocumentIsReplaced(t *testing.T) {
	s := Store{Root: t.TempDir()}
	writeFile(t, s.poolPath(), "mailru "+docB+"\nmailru "+docC+"\n")
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	fakeEgressCheck(t, testURL)
	now := time.Now()

	changes, changed, err := s.checkDocs(now)
	if err != nil || len(changes) != 0 || changed {
		t.Fatalf("one bad answer acted on: %+v %v %v", changes, changed, err)
	}
	changes, changed, err = s.checkDocs(now.Add(docCheckEvery))
	if err != nil || len(changes) != 1 || !changed {
		t.Fatalf("second check: %+v %v %v", changes, changed, err)
	}
	ch := changes[0]
	if ch.Client != "phone" || ch.New == nil || ch.New.URL != docB || ch.Err != nil || ch.Reason == "" {
		t.Errorf("change %+v", ch)
	}
	c, _ := s.Get("phone")
	if c.URL != docB || len(c.Docs()) != 1 || c.context() != testURL {
		t.Errorf("client after: %+v (the context stays the first document)", c)
	}
	st, _ := s.loadDocsState()
	if d := st.Docs[testURL]; d == nil || d.State != stateDead || d.Client != "phone" {
		t.Errorf("dead list: %+v", st.Docs)
	}
	if d, err := s.freeDoc(c); err != nil || d.URL != docC {
		t.Errorf("next free %+v %v", d, err)
	}
	if msg, _ := docChangeMessage(ch); msg != "docs.dead.replaced" {
		t.Errorf("message %s", msg)
	}
}

// Without a free document, a dead backup goes and the client keeps its
// working one; a dead only document stays, and the owner hears why.
func TestDeadDocumentWithoutAFreeOne(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDoc("phone", Doc{"mailru", docB}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("tablet", "mailru", docC); err != nil {
		t.Fatal(err)
	}
	fakeEgressCheck(t, docB, docC)
	now := time.Now()
	s.checkDocs(now)
	changes, changed, err := s.checkDocs(now)
	if err != nil || len(changes) != 2 || !changed {
		t.Fatalf("changes %+v %v %v", changes, changed, err)
	}
	for _, ch := range changes {
		switch ch.Client {
		case "phone":
			if ch.New != nil || ch.Err != nil {
				t.Errorf("phone %+v", ch)
			}
			if c, _ := s.Get("phone"); len(c.Docs()) != 1 || c.URL != testURL {
				t.Errorf("phone keeps its working document: %+v", c)
			}
			if m, _ := docChangeMessage(ch); m != "docs.dead.dropped" {
				t.Errorf("phone message %s", m)
			}
		case "tablet":
			if ch.New != nil || ch.Err == nil {
				t.Errorf("tablet %+v", ch)
			}
			if c, _ := s.Get("tablet"); c.URL != docC {
				t.Errorf("tablet lost its only document: %+v", c)
			}
			if m, _ := docChangeMessage(ch); m != "docs.dead.nofree" {
				t.Errorf("tablet message %s", m)
			}
		}
	}
}

// A free pool document that died is not handed out; checked again and
// open, it comes back after the quarantine.
func TestDeadPoolDocumentAndRecheck(t *testing.T) {
	s := Store{Root: t.TempDir()}
	writeFile(t, s.poolPath(), "mailru "+docB+"\n")
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	fakeEgressCheck(t, docB)
	now := time.Now()
	s.checkDocs(now)
	if changes, _, _ := s.checkDocs(now); len(changes) != 1 || changes[0].Client != "" {
		t.Fatalf("changes %+v", changes)
	}
	c, _ := s.Get("phone")
	if _, err := s.freeDoc(c); err == nil {
		t.Error("a dead document handed out")
	}
	if back, still, err := s.recheckDead(nil, now); err != nil || back != 0 || still != 1 {
		t.Errorf("still dead: %d %d %v", back, still, err)
	}

	fakeEgressCheck(t)
	if back, still, err := s.recheckDead([]string{docB}, now); err != nil || back != 1 || still != 0 {
		t.Fatalf("recheck: %d %d %v", back, still, err)
	}
	st, _ := s.loadDocsState()
	if d := st.Docs[docB]; d == nil || d.State != stateQuarantine || !d.Until.Equal(now.Add(docQuarantine)) {
		t.Fatalf("after the recheck %+v", st.Docs[docB])
	}
	if _, err := s.freeDoc(c); err == nil {
		t.Error("handed out during the quarantine")
	}
	if st.blocked(docB, now.Add(docQuarantine+time.Minute)) {
		t.Error("still blocked after the quarantine")
	}
	if len(st.deadDocs()) != 0 {
		t.Errorf("dead list %+v", st.deadDocs())
	}
}

// A revoked client's documents go back to the pool after the quarantine.
func TestRevokeQuarantinesDocuments(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := s.Revoke("phone", now); err != nil {
		t.Fatal(err)
	}
	pool, _ := s.poolStatus()
	if len(pool) != 1 || pool[0].URL != testURL || pool[0].free() || pool[0].State == nil || pool[0].State.State != stateQuarantine {
		t.Errorf("pool after revoke %+v", pool)
	}
}

// The bot's documents screen lists the dead ones, and its button checks
// one again.
func TestBotPoolScreen(t *testing.T) {
	s, f, owner, cb := bots(t)
	owner.clientBot = cb
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	c, _ := s.Get("phone")
	c.Telegram = &TGAccount{ID: 101, Username: "dima"}
	s.Save(c)
	fakeEgressCheck(t, testURL)
	now := time.Now()
	owner.docsTick(now)
	owner.docsTick(now.Add(docCheckEvery))
	var told []string
	for _, m := range f.to(42) {
		told = append(told, m.Text)
	}
	if !strings.Contains(strings.Join(told, "\n"), "больше не открывается") {
		t.Errorf("owner not told: %q", told)
	}
	if m := f.to(101); len(m) == 0 || !strings.Contains(m[0].Text, "переехал на новый документ") || f.photoCount() == 0 {
		t.Errorf("client not given the new link: %+v", m)
	}
	sc := owner.poolScreen()
	kb, _ := json.Marshal(sc.kb)
	if !strings.Contains(sc.text, testURL) || !strings.Contains(sc.text, "был у phone") || !strings.Contains(string(kb), `"pk:`+docKey(testURL)+`"`) {
		t.Errorf("screen:\n%s\n%s", sc.text, kb)
	}
	fakeEgressCheck(t)
	sc = owner.poolPress("pk", docKey(testURL))
	if !strings.Contains(sc.text, "Ожили: 1") || strings.Contains(sc.text, "<code>"+testURL) {
		t.Errorf("after the recheck:\n%s", sc.text)
	}
}

// reflux pool lists the states, and pool check / recheck act on them.
func TestPoolCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	s := Store{Root: home}
	writeFile(t, s.poolPath(), "mailru "+docB+"\nmailru "+docC+"\n")
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	fakeEgressCheck(t, docC)
	var out strings.Builder
	for i := 0; i < 2; i++ {
		out.Reset()
		if err := run([]string{"pool", "check"}, strings.NewReader(""), &out); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(out.String(), "no longer opens") {
		t.Errorf("check:\n%s", out.String())
	}
	out.Reset()
	if err := run([]string{"pool"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Dead (reflux pool recheck") || !strings.Contains(out.String(), "1 of 2 free") {
		t.Errorf("pool:\n%s", out.String())
	}
	fakeEgressCheck(t)
	out.Reset()
	if err := run([]string{"pool", "recheck"}, strings.NewReader(""), &out); err != nil || !strings.Contains(out.String(), "Back: 1") {
		t.Errorf("recheck: %v\n%s", err, out.String())
	}
}
