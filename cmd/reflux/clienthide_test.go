package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/telegram"
)

// Hidden, the client bot keeps quiet to strangers and still serves the
// people it deals with: a request waiting, a client, an invite.
func TestClientBotHiddenFromStrangers(t *testing.T) {
	s, f, owner, cb := bots(t)
	writeJSON(s.clientBotPath(), clientBotConfig{Token: testToken, Username: "reflux_access_bot"})
	// dima asks while the bot is shown to everyone.
	cb.handle(personPress(1, dima, "ask"))
	cb.handle(personMsg(2, dima, "Дима"))
	if r, err := s.getRequest(101); err != nil || r.State != reqPending {
		t.Fatalf("request %+v %v", r, err)
	}

	// The owner hides it in the bot's settings.
	owner.handle(press(3, 42, "set", time.Now()))
	if got := f.lastEditText(); !strings.Contains(got, "Бот клиентов: отвечает всем") {
		t.Fatalf("settings:\n%s", got)
	}
	owner.handle(press(4, 42, "cbhide", time.Now()))
	if !s.clientBotHidden() || !strings.Contains(f.lastEditText(), "скрыт от чужих") {
		t.Fatalf("not hidden:\n%s", f.lastEditText())
	}
	if c, _, _ := s.loadClientBot(); c.Token != testToken || c.Username != "reflux_access_bot" {
		t.Errorf("the switch lost the bot's settings: %+v", c)
	}

	stranger := telegram.User{ID: 202, Username: "x", LanguageCode: "ru"}
	n, edits := len(f.messages()), f.lastEditText()
	cb.handle(personMsg(5, stranger, "/start"))
	cb.handle(personMsg(6, stranger, "/start NOSUCHCODE1"))
	cb.handle(personPress(7, stranger, "ask"))
	if len(f.messages()) != n || f.lastEditText() != edits || len(f.to(202)) != 0 {
		t.Fatalf("answered a stranger: %+v", f.to(202))
	}
	if _, err := s.getRequest(202); err == nil {
		t.Error("a stranger filed a request")
	}

	// A request waiting is answered.
	cb.handle(personMsg(8, dima, "/start"))
	if m := f.to(101); !strings.Contains(m[len(m)-1].Text, "Заявка") {
		t.Errorf("dima, waiting: %q", m[len(m)-1].Text)
	}
	// An invite is answered, and its person is a client from then on.
	inv, err := s.newInvite("+30", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ann := telegram.User{ID: 303, Username: "ann", FirstName: "Ann", LanguageCode: "ru"}
	cb.handle(personMsg(9, ann, "/start "+inv.Code))
	if m := f.to(303); len(m) == 0 || !strings.Contains(m[0].Text, "Доступ открыт") {
		t.Fatalf("invite: %+v", m)
	}
	n = len(f.to(303))
	cb.handle(personMsg(10, ann, "/start"))
	if len(f.to(303)) != n+1 {
		t.Error("the invited client is not answered")
	}

	// Shown again from the CLI; the stranger gets the intro.
	var out strings.Builder
	if err := run([]string{"bot", "client", "show"}, nil, &out); err != nil || s.clientBotHidden() {
		t.Fatalf("show: %v %s", err, out.String())
	}
	cb.handle(personMsg(11, stranger, "/start"))
	if m := f.to(202); len(m) != 1 || !strings.Contains(m[0].Text, "Запросить доступ") {
		t.Errorf("stranger, shown: %+v", m)
	}
	if err := run([]string{"bot", "client", "hide"}, nil, &out); err != nil || !s.clientBotHidden() {
		t.Errorf("hide: %v %s", err, out.String())
	}
}

func TestClientBotHiddenOnThePanel(t *testing.T) {
	wt := newWebTest(t, "192.168.1.50")
	if body := wt.do("GET", "/requests", nil).Body.String(); strings.Contains(body, "/clientbot/") {
		t.Error("a switch without a client bot")
	}
	if rec := wt.do("POST", "/clientbot/hide", url.Values{}); rec.Code != http.StatusBadRequest {
		t.Errorf("hid a client bot that is not there: %d", rec.Code)
	}
	writeJSON(wt.s.clientBotPath(), clientBotConfig{Token: testToken})
	body := wt.do("GET", "/requests", nil).Body.String()
	if !strings.Contains(body, `action="/clientbot/hide"`) || !strings.Contains(body, "отвечает всем") {
		t.Fatalf("requests page:\n%s", body)
	}
	if rec := wt.do("POST", "/clientbot/hide", url.Values{}); rec.Code != http.StatusSeeOther || !strings.HasSuffix(rec.Header().Get("Location"), "?ok=cbot_hide") {
		t.Fatalf("hide: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	body = wt.do("GET", "/requests?ok=cbot_hide", nil).Body.String()
	if !wt.s.clientBotHidden() || !strings.Contains(body, `action="/clientbot/show"`) || !strings.Contains(body, "молчит для чужих") {
		t.Errorf("hidden page:\n%s", body)
	}
	wt.do("POST", "/clientbot/show", url.Values{})
	if wt.s.clientBotHidden() {
		t.Error("not shown again")
	}
}
