package main

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestWebAddPage(t *testing.T) {
	wt := newWebTest(t, "192.168.1.50")
	writeFile(t, wt.s.poolPath(), "yandex "+docY+"\nmailru "+docB+"\nmailru "+docC+"\n")
	if _, err := wt.s.Add("client1", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	c, _ := wt.s.Get("client1")
	c.Telegram = &TGAccount{ID: 42, Username: "owner", Name: "Owner"}
	wt.s.Save(c)
	body := wt.do("GET", "/add", nil).Body.String()
	for _, want := range []string{
		`placeholder="client2"`,                                      // a free name
		"Из пула (свободно 3)",                                       // the pool's free documents
		`<option value="` + docB + `">mailru · …/BBBB/bbbb</option>`, // mail.ru first
		`<option value="42">Owner (@owner)</option>`,                 // a known account
		`<option value="3">3</option>`,                               // up to 3 backups
		`name="how" value="+30"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("add page lacks %q", want)
		}
	}
	if strings.Index(body, docB) > strings.Index(body, docY) {
		t.Error("the Yandex document is offered before mail.ru")
	}
	if m := rawIDRe.FindString(body); m != "" || strings.Contains(body, "%!") {
		t.Errorf("add page: raw id %q or a format error", m)
	}
}

func TestWebAddFromThePool(t *testing.T) {
	wt := newWebTest(t, "192.168.1.50")
	writeFile(t, wt.s.poolPath(), "mailru "+docB+"\nmailru "+docC+"\nyandex "+docY+"\n")
	if _, err := wt.s.Add("me", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	me, _ := wt.s.Get("me")
	me.Telegram = &TGAccount{ID: 42, Username: "owner"}
	wt.s.Save(me)
	form := url.Values{"name": {"friend"}, "note": {"  Dima's   phone "}, "src": {"pool"}, "pooldoc": {docC},
		"backups": {"1"}, "how": {"+30"}, "tg": {"42"}} // no "start": added paused
	rec := wt.do("POST", "/add", form)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/c/friend?ok=added" {
		t.Fatalf("add: %d %s\n%s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	c, err := wt.s.Get("friend")
	if err != nil {
		t.Fatal(err)
	}
	if c.URL != docC || len(c.Backups) != 1 || c.Backups[0].URL != docB {
		t.Errorf("documents %+v", c.Docs())
	}
	if c.Note != "Dima's phone" || !c.Paused || c.Telegram == nil || c.Telegram.ID != 42 {
		t.Errorf("client %+v", c)
	}
	if d := time.Until(c.Expires); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Errorf("expires in %v, want 30 days", d)
	}
	// The note shows on the home page and the client page, and changes there.
	if body := wt.do("GET", "/", nil).Body.String(); !strings.Contains(body, "Dima&#39;s phone") {
		t.Error("home lacks the note")
	}
	if rec := wt.do("POST", "/c/friend/note", url.Values{"note": {"brother"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("note: %d", rec.Code)
	}
	if c, _ := wt.s.Get("friend"); c.Note != "brother" {
		t.Errorf("note %q", c.Note)
	}
	if body := wt.do("GET", "/c/friend?ok=note", nil).Body.String(); !strings.Contains(body, "<dd>brother</dd>") || !strings.Contains(body, "Заметка сохранена") {
		t.Error("client page lacks the note")
	}
}

func TestWebAddByLinkAndRefusals(t *testing.T) {
	wt := newWebTest(t, "192.168.1.50")
	writeFile(t, wt.s.poolPath(), "mailru "+docB+"\n")
	// A link typed in wins over the pool's choice; the type can be given.
	rec := wt.do("POST", "/add", url.Values{"name": {"board"}, "src": {"pool"}, "pooldoc": {docB},
		"url": {"https://disk.yandex.ru/i/board"}, "type": {"boards"}, "how": {"date"}, "date": {"2030-01-31"}, "start": {"1"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("add: %d\n%s", rec.Code, rec.Body)
	}
	c, _ := wt.s.Get("board")
	if c.URL != "https://disk.yandex.ru/i/board" || c.Transport != "boards" || c.Paused || c.Expires.Local().Format("2006-01-02") != "2030-02-01" {
		t.Errorf("by link: %+v", c)
	}
	for name, form := range map[string]url.Values{
		"taken pool document": {"name": {"a"}, "src": {"pool"}, "pooldoc": {"https://cloud.mail.ru/public/Gone/x"}},
		"too many backups":    {"name": {"b"}, "src": {"pool"}, "pooldoc": {docB}, "backups": {"1"}},
		"a long note":         {"name": {"c"}, "url": {docC}, "note": {strings.Repeat("я", 101)}},
		"an unknown account":  {"name": {"d"}, "url": {docC}, "tg": {"7"}},
		"no document":         {"name": {"e"}, "src": {"url"}},
		"a bad date":          {"name": {"f"}, "url": {docC}, "how": {"date"}, "date": {"soon"}},
	} {
		if rec := wt.do("POST", "/add", form); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rec.Code)
		}
		if _, err := wt.s.Get(form.Get("name")); err == nil {
			t.Errorf("%s: the client was added", name)
		}
	}
}

func TestNoteInTheBotAndTheCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	if err := run([]string{"add", "phone", "--url", testURL, "--no-apply", "--note", "my  phone"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	s := Store{Root: home}
	if c, _ := s.Get("phone"); c.Note != "my phone" {
		t.Errorf("note %q", c.Note)
	}
	s.saveBotConfig(botConfig{Token: testToken, Chat: 42})
	f := newFakeTG(t)
	c, _ := s.loadBotConfig()
	b := newBot(s, c)
	b.handle(press(1, 42, "c:phone", time.Now()))
	if got := f.lastEditText(); !strings.Contains(got, "📝 my phone") {
		t.Errorf("client card:\n%s", got)
	}
	b.handle(press(2, 42, "cls", time.Now()))
	if got := f.lastEditText(); !strings.Contains(got, "<i>my phone</i>") {
		t.Errorf("clients screen:\n%s", got)
	}
	// The list: the note, and the backup documents after the transport.
	s.AddDoc("phone", Doc{"mailru", docB})
	var out strings.Builder
	if err := run([]string{"list"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "mailru+1") || !strings.Contains(out.String(), "my phone") {
		t.Errorf("list:\n%s", out.String())
	}
}
