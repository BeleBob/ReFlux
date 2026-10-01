package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openflux/share"
	"openflux/transport/ipc"
)

const (
	docB = "https://cloud.mail.ru/public/BBBB/bbbb"
	docC = "https://cloud.mail.ru/public/CCCC/cccc"
	docY = "https://disk.yandex.ru/i/YYYY"
)

func TestClientDocuments(t *testing.T) {
	s := Store{Root: t.TempDir()}
	c, err := s.Add("phone", "mailru", testURL)
	if err != nil {
		t.Fatal(err)
	}
	if c.session() || len(c.Docs()) != 1 {
		t.Fatalf("a new client: %+v", c)
	}
	if _, err := s.Add("tablet", "mailru", testURL); err == nil {
		t.Error("a second client on phone's document")
	}
	if _, err := s.AddDoc("phone", Doc{"mailru", testURL}); err == nil {
		t.Error("the main document again as a backup")
	}
	if _, err := s.AddDoc("phone", Doc{"mailru", "https://x/#y"}); err == nil {
		t.Error("a URL the .conf would cut")
	}
	if _, err := s.AddDoc("phone", Doc{"oneme", docB}); err == nil {
		t.Error("an unsupported transport")
	}
	if c, err = s.AddDoc("phone", Doc{"mailru", docB}); err != nil || !c.session() || c.context() != testURL {
		t.Fatalf("backup: %+v %v", c, err)
	}
	if _, err := s.Add("tablet", "mailru", docB); err == nil {
		t.Error("a client on phone's backup document")
	}
	for i := 0; len(c.Docs()) < maxDocs; i++ {
		if c, err = s.AddDoc("phone", Doc{"mailru", fmt.Sprintf("https://cloud.mail.ru/public/X%d/x", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddDoc("phone", Doc{"mailru", docC}); err == nil {
		t.Errorf("more than %d documents", maxDocs)
	}
	// Removing a backup.
	if c, err = s.RemoveDoc("phone", 4); err != nil || len(c.Docs()) != 4 || c.Context != "" {
		t.Fatalf("remove a backup: %+v %v", c, err)
	}
	if _, err := s.RemoveDoc("phone", 9); err == nil {
		t.Error("removed a document that is not there")
	}
	// Removing the main one: the first backup takes its place, the
	// context stays.
	if c, err = s.RemoveDoc("phone", 0); err != nil {
		t.Fatal(err)
	}
	if c.URL != docB || c.Context != testURL || c.context() != testURL || len(c.Backups) != 2 {
		t.Fatalf("main removed: %+v", c)
	}
	for len(c.Docs()) > 1 {
		if c, err = s.RemoveDoc("phone", 1); err != nil {
			t.Fatal(err)
		}
	}
	// One document left, but the context is not its URL: still a Session.
	if !c.session() || c.Backups != nil {
		t.Errorf("one document, another context: %+v", c)
	}
	if _, err := s.RemoveDoc("phone", 0); err == nil {
		t.Error("removed the last document")
	}
	// The first document back as the main one: classic again.
	if c, err = s.AddDoc("phone", Doc{"mailru", testURL}); err != nil {
		t.Fatal(err)
	}
	if c, err = s.RemoveDoc("phone", 0); err != nil || c.session() || c.URL != testURL {
		t.Errorf("back on the first document: %+v %v", c, err)
	}
	got, _ := s.Get("phone")
	if got.URL != testURL || got.Context != "" || got.Backups != nil {
		t.Errorf("saved %+v", got)
	}
}

func TestSessionNodeConfAndLink(t *testing.T) {
	c := Client{Name: "phone", Transport: "mailru", URL: testURL}
	if conf := nodeConf(c); !strings.Contains(conf, "Transport = mailru\nURL = "+testURL) || strings.Contains(conf, "[Transport") {
		t.Errorf("one document is a classic exit:\n%s", conf)
	}
	c.Backups = []Doc{{"mailru", docB}, {"yandex", docY}}
	want := "[Interface]\nRole = exit\nMode = l4\nURL = " + testURL + "\n" +
		"EncryptionKeyFile = /config/key\nCookieStore = /state/cookies.json\nIPCSocket = /state/ipc.sock\n\n" +
		"[Transport \"mailru\"]\nType = mailru\nPriority = 100\nURL = " + testURL + "\n\n" +
		"[Transport \"mailru-2\"]\nType = mailru\nPriority = 90\nURL = " + docB + "\n\n" +
		"[Transport \"yandex\"]\nType = yandex\nPriority = 80\nURL = " + docY + "\n"
	if conf := nodeConf(c); !strings.HasSuffix(conf, want) {
		t.Errorf("Session node.conf:\n%s\nwant the end:\n%s", conf, want)
	}
	if c.docIndex("mailru-2") != 1 || c.docIndex("yandex") != 2 || c.docIndex("") != -1 {
		t.Error("carrier names do not map back to documents")
	}
	// The link the app imports: the same carriers, priorities and context.
	key := strings.Repeat("k", 32)
	link, err := share.MakeLink(shareConfig(c, key))
	if err != nil {
		t.Fatal(err)
	}
	got, err := share.Decode(link)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Negotiate || got.Context != testURL || got.Secret != key || len(got.Transports) != 3 {
		t.Fatalf("link %+v", got)
	}
	for i, want := range []share.Transport{
		{Type: "mailru", URL: testURL, Priority: 100},
		{Type: "mailru", Name: "mailru-2", URL: docB, Priority: 90},
		{Type: "yandex", URL: docY, Priority: 80},
	} {
		if got.Transports[i] != want {
			t.Errorf("transport %d = %+v, want %+v", i, got.Transports[i], want)
		}
	}
	// The main document replaced: the node and the link keep the context.
	c.URL, c.Backups, c.Context = docB, nil, testURL
	if conf := nodeConf(c); !strings.Contains(conf, "URL = "+testURL+"\n") || !strings.Contains(conf, "[Transport \"mailru\"]\nType = mailru\nPriority = 100\nURL = "+docB) {
		t.Errorf("the context kept in node.conf:\n%s", conf)
	}
	if cfg := shareConfig(c, key); !cfg.Negotiate || cfg.Context != testURL {
		t.Errorf("the context kept in the link: %+v", cfg)
	}
}

func TestDocumentPool(t *testing.T) {
	s := Store{Root: t.TempDir()}
	writeFile(t, s.poolPath(), "# prepared on 2026-09-30\n"+
		"yandex "+docY+"\n\n"+
		"mailru "+testURL+"\n"+
		docB+"\n"+
		"mailru "+docC+"\n")
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	// A revoked client's document is not handed out again.
	writeFile(t, filepath.Join(s.Root, "revoked", "old-20260901-000000", "client.json"), `{"name":"old","transport":"mailru","url":"`+docC+`"}`)
	docs, err := s.poolStatus()
	if err != nil || len(docs) != 4 {
		t.Fatalf("pool %+v %v", docs, err)
	}
	if docs[0].Transport != "yandex" || docs[1].User != "phone" || docs[2].Transport != "mailru" || docs[2].User != "" || docs[3].User != "old-20260901-000000" {
		t.Errorf("pool status %+v", docs)
	}
	c, _ := s.Get("phone")
	if d, err := s.freeDoc(c); err != nil || d.URL != docB {
		t.Errorf("free document %+v %v, want the mail.ru one before Yandex", d, err)
	}
	if n := s.freeCount(); n != 2 {
		t.Errorf("%d free", n)
	}
	s.AddDoc("phone", Doc{"mailru", docB})
	c, _ = s.Get("phone")
	if d, err := s.freeDoc(c); err != nil || d.URL != docY {
		t.Errorf("then %+v %v, want the Yandex one", d, err)
	}
	s.AddDoc("phone", Doc{"yandex", docY})
	c, _ = s.Get("phone")
	if _, err := s.freeDoc(c); err == nil {
		t.Error("a free document from a used-up pool")
	}
	// Adding to the pool: new links only, checked, the file stays 0600.
	if n, err := s.addToPool([]Doc{{"mailru", docB}, {"mailru", "https://cloud.mail.ru/public/N/n"}}); err != nil || n != 1 {
		t.Errorf("added %d, %v", n, err)
	}
	if _, err := s.addToPool([]Doc{{"mailru", "https://x/;y"}}); err == nil {
		t.Error("a bad URL went into the pool")
	}
	if fi, _ := os.Stat(s.poolPath()); fi.Mode().Perm() != 0o600 {
		t.Errorf("pool mode %v", fi.Mode())
	}
	if docs, _ := s.pool(); len(docs) != 5 || docs[4].URL != "https://cloud.mail.ru/public/N/n" {
		t.Errorf("pool after adding: %+v", docs)
	}
	writeFile(t, s.poolPath(), "mailru a b\n")
	if _, err := s.pool(); err == nil {
		t.Error("a bad pool line read")
	}
}

func TestDocsCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	calls := fakeDocker(t, "")
	if err := run([]string{"add", "phone", "--url", testURL, "--no-apply"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	s := Store{Root: home}
	writeFile(t, s.poolPath(), docB+"\n")
	var out strings.Builder
	if err := run([]string{"pool"}, nil, &out); err != nil || !strings.Contains(out.String(), "mailru  free") || !strings.Contains(out.String(), "1 of 1 free") {
		t.Errorf("pool: %v\n%s", err, out.String())
	}
	*calls = nil
	out.Reset()
	if err := run([]string{"docs", "phone", "add", "pool"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "2  mailru  backup "+docB) || !strings.Contains(out.String(), "Session exit") || !strings.Contains(out.String(), "reflux show phone") {
		t.Errorf("docs add:\n%s", out.String())
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "up --detach") {
		t.Errorf("the node was not restarted: %q", *calls)
	}
	conf, _ := os.ReadFile(filepath.Join(home, "clients", "phone", "node.conf"))
	if !strings.Contains(string(conf), `[Transport "mailru-2"]`) {
		t.Errorf("node.conf:\n%s", conf)
	}
	out.Reset()
	if err := run([]string{"show", "phone"}, nil, &out); err != nil || !strings.Contains(out.String(), "Session (several documents") || !strings.Contains(out.String(), "mailru-2        mailru, priority 90: "+docB) {
		t.Errorf("show:\n%s", out.String())
	}
	if err := run([]string{"docs", "phone", "remove", "x"}, nil, io.Discard); err == nil {
		t.Error("removed document x")
	}
	out.Reset()
	if err := run([]string{"docs", "phone", "remove", "2"}, nil, &out); err != nil || !strings.Contains(out.String(), "classic exit") {
		t.Errorf("docs remove: %v\n%s", err, out.String())
	}
	if err := run([]string{"docs", "phone", "add", "https://disk.yandex.ru/i/Q", "--no-apply"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Get("phone"); len(c.Backups) != 1 || c.Backups[0].Transport != "yandex" {
		t.Errorf("the type from the link: %+v", c.Backups)
	}
}

func TestDocInUseCheck(t *testing.T) {
	docSeen.Lock()
	docSeen.m = map[string]int{}
	docSeen.Unlock()
	c := Client{Name: "phone", Transport: "mailru", URL: testURL, Backups: []Doc{{"mailru", docB}}}
	check := func(c Client, st ipc.StatusPayload) []finding {
		d := &doctor{}
		d.docInUse(c, st)
		return d.findings
	}
	if fs := check(c, ipc.StatusPayload{Running: true}); len(fs) != 0 {
		t.Errorf("never seen online: %+v", fs)
	}
	fs := check(c, ipc.StatusPayload{Running: true, Connected: true, Active: "mailru"})
	if len(fs) != 1 || fs[0].Level != levelOK || fs[0].Msg != "node.onmain" || category(fs[0].Key) != "nodes" {
		t.Fatalf("on the main document: %+v", fs)
	}
	fs = check(c, ipc.StatusPayload{Running: true, Connected: true, Active: "mailru-2"})
	if fs[0].Level != levelWarn || fs[0].text(langRU) != "нода phone: клиент на резервном документе 2 — основной до него не доходит (reflux docs phone)" {
		t.Errorf("on the backup: %+v %q", fs, fs[0].text(langRU))
	}
	// The phone asleep: the warning stays, no resolved-and-raised-again.
	if fs := check(c, ipc.StatusPayload{Running: true}); fs[0].Level != levelWarn {
		t.Errorf("client away: %+v", fs)
	}
	if fs := check(Client{Name: "laptop", Transport: "mailru", URL: docC}, ipc.StatusPayload{Connected: true}); len(fs) != 0 {
		t.Errorf("a classic client: %+v", fs)
	}
}

func TestBotDocuments(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	s := Store{Root: home}
	s.saveBotConfig(botConfig{Token: testToken, Chat: 42})
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	writeFile(t, s.poolPath(), "mailru "+docB+"\n")
	f := newFakeTG(t)
	c, _ := s.loadBotConfig()
	b := newBot(s, c)
	b.handle(press(1, 42, "c:phone", time.Now()))
	if !strings.Contains(f.lastEditText(), "phone") {
		t.Fatalf("client card:\n%s", f.lastEditText())
	}
	b.handle(press(2, 42, "dc:phone", time.Now()))
	if got := f.lastEditText(); !strings.Contains(got, "1. mailru · основной") || !strings.Contains(got, "Свободно в пуле: 1") {
		t.Errorf("documents screen:\n%s", got)
	}
	b.handle(press(3, 42, "dp:phone", time.Now()))
	if got := f.lastEditText(); !strings.Contains(got, "Документ 2 добавлен") || !strings.Contains(got, "2. mailru · резерв") || !strings.Contains(got, "Документов: 2 — нода в режиме Session") {
		t.Errorf("after the pool:\n%s", got)
	}
	// By link: the bot waits for it.
	b.handle(press(4, 42, "du:phone", time.Now()))
	b.handle(msg(5, 42, docY))
	if c, _ := s.Get("phone"); len(c.Docs()) != 3 || c.Backups[1] != (Doc{"yandex", docY}) {
		t.Fatalf("by link: %+v", c)
	}
	// Removing asks first; a stale confirmation does nothing.
	b.handle(press(6, 42, "dr:phone:1", time.Now()))
	if !strings.Contains(f.lastEditText(), "Убрать документ 2") {
		t.Errorf("ask:\n%s", f.lastEditText())
	}
	old := fmt.Sprint(time.Now().Add(-time.Hour).Unix())
	b.handle(press(7, 42, "dr!:phone:1:"+old, time.Now()))
	if c, _ := s.Get("phone"); len(c.Docs()) != 3 {
		t.Error("a stale button removed a document")
	}
	b.handle(press(8, 42, "dr!:phone:1:"+stamp(), time.Now()))
	if c, _ := s.Get("phone"); len(c.Docs()) != 2 || c.Backups[0].URL != docY {
		t.Errorf("removed: %+v", c)
	}
	if !strings.Contains(f.lastEditText(), "Документ 2 убран") {
		t.Errorf("after removing:\n%s", f.lastEditText())
	}
}

func TestWebDocuments(t *testing.T) {
	wt := newWebTest(t, "192.168.1.50")
	if _, err := wt.s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	writeFile(t, wt.s.poolPath(), "mailru "+docB+"\n")
	body := wt.do("GET", "/c/phone", nil).Body.String()
	if !strings.Contains(body, "Из пула (свободно 1)") || strings.Contains(body, "docrm") {
		t.Errorf("client page, one document:\n%s", body)
	}
	if rec := wt.do("POST", "/c/phone/docadd", url.Values{}); rec.Code != http.StatusSeeOther || !strings.HasSuffix(rec.Header().Get("Location"), "?ok=docadded") {
		t.Fatalf("from the pool: %d %s\n%s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if rec := wt.do("POST", "/c/phone/docadd", url.Values{"url": {docC}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("by link: %d\n%s", rec.Code, rec.Body)
	}
	if rec := wt.do("POST", "/c/phone/docadd", url.Values{"url": {docC}}); rec.Code != http.StatusBadRequest {
		t.Errorf("the same document twice: %d", rec.Code)
	}
	body = wt.do("GET", "/c/phone", nil).Body.String()
	for _, want := range []string{docB, docC, "резерв · 90", "Документов: 3 — нода в режиме Session", `name="i" value="2"`} {
		if !strings.Contains(body, want) {
			t.Errorf("client page lacks %q", want)
		}
	}
	show := wt.do("POST", "/c/phone/show", url.Values{}).Body.String()
	if !strings.Contains(show, "<dd>Session</dd>") || !strings.Contains(show, "2. mailru · 90") {
		t.Errorf("show page:\n%s", show)
	}
	if rec := wt.do("POST", "/c/phone/docrm", url.Values{"i": {"0"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("remove: %d\n%s", rec.Code, rec.Body)
	}
	if c, _ := wt.s.Get("phone"); c.URL != docB || c.Context != testURL {
		t.Errorf("main removed: %+v", c)
	}
}

// lastEditText is the screen the bot last edited a message into.
func (f *fakeTG) lastEditText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.edited) == 0 {
		return ""
	}
	return f.edited[len(f.edited)-1]
}
