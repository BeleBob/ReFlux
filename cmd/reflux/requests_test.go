package main

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/telegram"
)

func TestRequestRules(t *testing.T) {
	s := Store{Root: t.TempDir()}
	now := time.Now()
	dima := telegram.User{ID: 101, Username: "Dima_K", FirstName: "Dima", LanguageCode: "ru"}
	r, err := s.newRequest(dima, reqAccess, "  brother,   phone ", now)
	if err != nil || r.State != reqPending || r.Text != "brother, phone" || r.Lang != "ru" {
		t.Fatalf("request %+v %v", r, err)
	}
	if _, err := s.newRequest(dima, reqAccess, "", now); !errors.Is(err, errWaiting) {
		t.Errorf("a second request: %v", err)
	}
	if _, err := s.newRequest(telegram.User{ID: 102}, reqExtend, "", now); err == nil {
		t.Error("more time asked for no channel")
	}
	// Rejected: again after a day only.
	s.decideRequest(101, reqRejected, now)
	if _, err := s.newRequest(dima, reqAccess, "", now.Add(time.Hour)); !errors.Is(err, errTooSoon) {
		t.Errorf("right after the rejection: %v", err)
	}
	if _, err := s.newRequest(dima, reqAccess, "", now.Add(25*time.Hour)); err != nil {
		t.Errorf("a day after the rejection: %v", err)
	}
	// Blocked: never.
	s.decideRequest(101, reqBlocked, now)
	if _, err := s.newRequest(dima, reqAccess, "", now.AddDate(1, 0, 0)); !errors.Is(err, errBlocked) {
		t.Errorf("blocked: %v", err)
	}
	// Too many waiting.
	for i := int64(0); i < maxPending; i++ {
		if _, err := s.newRequest(telegram.User{ID: 1000 + i}, reqAccess, "", now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.newRequest(telegram.User{ID: 5000}, reqAccess, "", now); !errors.Is(err, errTooMany) {
		t.Errorf("over the limit: %v", err)
	}
	// A long story is cut.
	s.removeRequest(1000)
	if r, _ := s.newRequest(telegram.User{ID: 6000}, reqAccess, strings.Repeat("a", 400), now); len([]rune(r.Text)) != 301 {
		t.Errorf("text of %d characters", len([]rune(r.Text)))
	}
}

func TestApproveRequests(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	s := Store{Root: home}
	writeFile(t, s.poolPath(), "yandex "+docY+"\nmailru "+docB+"\nmailru "+docC+"\n")
	now := time.Now()
	s.newRequest(telegram.User{ID: 101, Username: "Dima_K", FirstName: "Dima"}, reqAccess, "brother", now)
	r, c, err := s.approveRequest(101, "+30", now)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "dima-k" || c.URL != docB || c.Telegram == nil || c.Telegram.ID != 101 || c.Note != "brother" ||
		r.State != reqApproved || r.Client != "dima-k" {
		t.Errorf("approved %+v / %+v", c, r)
	}
	if d := c.Expires.Sub(now); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Errorf("expires in %v", d)
	}
	if _, _, err := s.approveRequest(101, "+30", now); err == nil {
		t.Error("approved twice")
	}
	if _, err := s.newRequest(telegram.User{ID: 101}, reqAccess, "", now); !errors.Is(err, errWaiting) {
		t.Errorf("before the link went out: %v", err)
	}
	// Names: a taken one gets a number; no username gives tg<id>.
	s.newRequest(telegram.User{ID: 102, Username: "dima_k"}, reqAccess, "", now)
	if _, c, _ := s.approveRequest(102, "never", now); c.Name != "dima-k-2" || !c.Expires.IsZero() {
		t.Errorf("second dima: %+v", c)
	}
	r103, _ := s.newRequest(telegram.User{ID: 103, FirstName: "Иван"}, reqAccess, "", now)
	if r103.Who() != "Иван (id 103)" {
		t.Errorf("who %q", r103.Who())
	}
	if _, c, err := s.approveRequest(103, "+90", now); err != nil || c.Name != "tg103" || c.URL != docY || c.Note != "" {
		t.Errorf("no username, no note: %+v %v", c, err)
	}
	// The pool used up: refused, no client made.
	s.newRequest(telegram.User{ID: 104, Username: "late"}, reqAccess, "", now)
	if _, _, err := s.approveRequest(104, "+30", now); err == nil {
		t.Error("approved without a free document")
	}
	if _, err := s.Get("late"); err == nil {
		t.Error("a client was made without a document")
	}
	// More time for a channel.
	c, _ = s.Get("dima-k")
	before := c.Expires
	if _, err := s.newRequest(telegram.User{ID: 101}, reqExtend, "please", now); err == nil {
		t.Error("an extension while the approved request is still there")
	}
	s.removeRequest(101) // the client bot does this once it has told them
	if _, err := s.newRequest(telegram.User{ID: 101}, reqAccess, "", now); !errors.Is(err, errHasChannel) {
		t.Errorf("a second channel: %v", err)
	}
	if r, err := s.newRequest(telegram.User{ID: 101}, reqExtend, "please", now); err != nil || r.Client != "dima-k" {
		t.Fatalf("extension request %+v %v", r, err)
	}
	if _, c, err := s.approveRequest(101, "+30", now); err != nil || !c.Expires.Equal(before.AddDate(0, 0, 30)) {
		t.Errorf("extended to %v, want %v (%v)", c.Expires, before.AddDate(0, 0, 30), err)
	}
}

func TestRequestsCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	calls := fakeDocker(t, "")
	s := Store{Root: home}
	writeFile(t, s.poolPath(), "mailru "+docB+"\n")
	now := time.Now()
	s.newRequest(telegram.User{ID: 101, Username: "dima"}, reqAccess, "brother", now)
	s.newRequest(telegram.User{ID: 102, Username: "spam"}, reqAccess, "", now)
	var out strings.Builder
	if err := run([]string{"requests"}, nil, &out); err != nil || !strings.Contains(out.String(), "pending   access  101") || !strings.Contains(out.String(), "brother") {
		t.Errorf("list: %v\n%s", err, out.String())
	}
	out.Reset()
	if err := run([]string{"requests", "approve", "@dima", "--expires", "90"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if c, err := s.Get("dima"); err != nil || c.Expires.Sub(now) < 89*24*time.Hour {
		t.Errorf("approved: %+v %v", c, err)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "up --detach") {
		t.Error("the node was not started")
	}
	if err := run([]string{"requests", "block", "102"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.getRequest(102); r.State != reqBlocked {
		t.Errorf("block: %+v", r)
	}
	if err := run([]string{"requests", "forget", "spam"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := s.newRequest(telegram.User{ID: 102, Username: "spam"}, reqAccess, "", now); err != nil {
		t.Errorf("forgotten, yet: %v", err)
	}
	if err := run([]string{"requests", "approve", "@nobody"}, nil, io.Discard); err == nil {
		t.Error("approved nobody")
	}
}

func TestRequestsInTheOwnerBot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	s := Store{Root: home}
	s.saveBotConfig(botConfig{Token: testToken, Chat: 42})
	writeFile(t, s.poolPath(), "mailru "+docB+"\n")
	f := newFakeTG(t)
	c, _ := s.loadBotConfig()
	b := newBot(s, c)
	s.newRequest(telegram.User{ID: 101, Username: "dima"}, reqAccess, "brother", time.Now())
	s.newRequest(telegram.User{ID: 102, Username: "spam"}, reqAccess, "", time.Now())
	b.notifyRequests()
	b.notifyRequests() // once each
	m := f.messages()
	if len(m) != 2 || !strings.Contains(m[0].Text, "<b>@dima</b> просит доступ") || !strings.Contains(m[0].Text, "«brother»") ||
		!strings.Contains(m[0].Buttons.String(), "rq:101:+30") {
		t.Fatalf("notifications %+v", m)
	}
	if evs := s.readEvents(5); len(evs) != 2 || !strings.Contains(evs[0].RU, "Заявка на доступ от") {
		t.Errorf("events %+v", evs)
	}
	if sc := b.home(); !strings.Contains(sc.kb[0][0].Text, "Заявки: 2") {
		t.Errorf("home keyboard %+v", sc.kb[0])
	}
	b.handle(press(1, 42, "rq:101:+30", time.Now()))
	if got := f.lastEditText(); !strings.Contains(got, "Одобрено: клиент <b>dima</b>") {
		t.Errorf("approve:\n%s", got)
	}
	if c, err := s.Get("dima"); err != nil || c.Telegram == nil || c.Telegram.Username != "dima" {
		t.Errorf("client %+v %v", c, err)
	}
	b.handle(press(2, 42, "rq:102:blk", time.Now()))
	if r, _ := s.getRequest(102); r.State != reqBlocked {
		t.Errorf("blocked: %+v", r)
	}
	// A request decided elsewhere: the buttons say so.
	b.handle(press(3, 42, "rq:101:+90", time.Now()))
	if got := f.lastEditText(); !strings.Contains(got, "Решение: одобрено") {
		t.Errorf("decided twice:\n%s", got)
	}
	b.handle(msg(4, 42, "/requests"))
	if m := f.messages(); !strings.Contains(m[len(m)-1].Text, "ждут решения: 0") {
		t.Errorf("/requests: %q", m[len(m)-1].Text)
	}
}

func TestRequestsOnThePanel(t *testing.T) {
	wt := newWebTest(t, "192.168.1.50")
	writeFile(t, wt.s.poolPath(), "mailru "+docB+"\n")
	now := time.Now()
	wt.s.newRequest(telegram.User{ID: 101, Username: "dima", FirstName: "Dima"}, reqAccess, "brother", now)
	wt.s.newRequest(telegram.User{ID: 102, Username: "spam"}, reqAccess, "", now)
	body := wt.do("GET", "/", nil).Body.String()
	if !strings.Contains(body, `<b class="badge">2</b>`) {
		t.Error("the menu does not count the requests")
	}
	body = wt.do("GET", "/requests", nil).Body.String()
	for _, want := range []string{"<b>Dima (@dima)</b>", "«brother»", `action="/requests/101/approve"`, "свободно 1"} {
		if !strings.Contains(body, want) {
			t.Errorf("requests page lacks %q", want)
		}
	}
	if m := rawIDRe.FindString(body); m != "" {
		t.Errorf("raw id %q", m)
	}
	rec := wt.do("POST", "/requests/101/approve", url.Values{"how": {"+90"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/requests?ok=rq_approved" {
		t.Fatalf("approve: %d %s\n%s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if c, err := wt.s.Get("dima"); err != nil || c.Telegram == nil || c.Expires.Sub(now) < 89*24*time.Hour {
		t.Errorf("client %+v %v", c, err)
	}
	if rec := wt.do("POST", "/requests/101/approve", url.Values{"how": {"+30"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("approved twice: %d", rec.Code)
	}
	wt.do("POST", "/requests/102/block", url.Values{})
	if r, _ := wt.s.getRequest(102); r.State != reqBlocked {
		t.Errorf("block: %+v", r)
	}
	body = wt.do("GET", "/requests?ok=rq_blocked", nil).Body.String()
	if !strings.Contains(body, "заблокирован") || !strings.Contains(body, `<a href="/c/dima">dima</a>`) || strings.Contains(body, `class="badge"`) {
		t.Errorf("after deciding:\n%s", body)
	}
	wt.do("POST", "/requests/102/forget", url.Values{})
	if _, err := wt.s.getRequest(102); err == nil {
		t.Error("not forgotten")
	}
}
