package main

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/telegram"
)

func TestInvites(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	s := Store{Root: home}
	writeFile(t, s.poolPath(), "mailru "+docB+"\nmailru "+docC+"\n")
	now := time.Now()
	inv, err := s.newInvite("+90", "  for   Oleg ", now)
	if err != nil || !inviteCodeRe.MatchString(inv.Code) || inv.Note != "for Oleg" || !inv.Expires.Equal(now.Add(inviteFor).UTC().Truncate(time.Second)) {
		t.Fatalf("invite %+v %v", inv, err)
	}
	if _, err := s.newInvite("soon", "", now); err == nil {
		t.Error("an invite for a bad access time")
	}
	// Someone who cannot use it leaves it be.
	s.newRequest(telegram.User{ID: 9}, reqAccess, "", now)
	s.decideRequest(9, reqBlocked, now)
	if _, _, _, err := s.useInvite(inv.Code, telegram.User{ID: 9}, now); !errors.Is(err, errBlocked) {
		t.Errorf("blocked: %v", err)
	}
	if len(s.listInvites(now)) != 1 {
		t.Fatal("a blocked account spent the invite")
	}
	// The one it is for gets a channel at once.
	r, c, used, err := s.useInvite(inv.Code, telegram.User{ID: 101, Username: "oleg"}, now)
	if err != nil || r.State != reqApproved || c.Name != "oleg" || c.Telegram == nil || c.Note != "for Oleg" || used.Code != inv.Code {
		t.Fatalf("used: %+v %+v %v", r, c, err)
	}
	if d := c.Expires.Sub(now); d < 89*24*time.Hour {
		t.Errorf("access for %v, want 90 days", d)
	}
	if _, _, _, err := s.useInvite(inv.Code, telegram.User{ID: 102}, now); !errors.Is(err, errNoInvite) {
		t.Errorf("used twice: %v", err)
	}
	// Expired ones go; revoked ones go; junk is no invite.
	old, _ := s.newInvite("+30", "", now.Add(-8*24*time.Hour))
	if len(s.listInvites(now)) != 0 {
		t.Error("an expired invite listed")
	}
	if _, _, _, err := s.useInvite(old.Code, telegram.User{ID: 103}, now); !errors.Is(err, errNoInvite) {
		t.Errorf("expired: %v", err)
	}
	inv2, _ := s.newInvite("never", "", now)
	if err := s.revokeInvite(inv2.Code); err != nil || s.revokeInvite(inv2.Code) == nil || s.revokeInvite("../../etc/passwd") == nil {
		t.Error("revoke")
	}
	// The pool used up: the person's request waits for the owner.
	s.Add("x1", "mailru", docC)
	inv3, _ := s.newInvite("+30", "", now)
	r, _, _, err = s.useInvite(inv3.Code, telegram.User{ID: 104, Username: "late"}, now)
	if err == nil || r.State != reqPending || len(s.listInvites(now)) != 0 {
		t.Errorf("no documents: %+v %v", r, err)
	}
}

func TestInviteInTheBots(t *testing.T) {
	s, f, owner, cb := bots(t)
	writeJSON(s.clientBotPath(), clientBotConfig{Token: testToken, Username: "reflux_access_bot"})
	// The owner makes one in his bot.
	owner.handle(press(1, 42, "rqs", time.Now()))
	owner.handle(press(2, 42, "inv:+30", time.Now()))
	got := f.lastEditText()
	invs := s.listInvites(time.Now())
	if len(invs) != 1 || !strings.Contains(got, "https://t.me/reflux_access_bot?start="+invs[0].Code) {
		t.Fatalf("invite screen:\n%s", got)
	}
	// The person opens the link.
	cb.handle(personMsg(3, dima, "/start "+invs[0].Code))
	m := f.to(101)
	if len(m) == 0 || !strings.Contains(m[0].Text, "Доступ выдан") {
		t.Fatalf("person: %+v", m)
	}
	if o := f.to(42); !strings.Contains(o[len(o)-1].Text, "По приглашению пришёл <b>Dima (@dima)</b>: клиент <b>dima</b>") {
		t.Errorf("owner: %q", o[len(o)-1].Text)
	}
	if _, err := s.getRequest(101); err == nil {
		t.Error("the delivered request stays")
	}
	// A spent or made-up code.
	cb.handle(personMsg(4, telegram.User{ID: 202, Username: "x"}, "/start "+invs[0].Code))
	if m := f.to(202); !strings.Contains(m[0].Text, "не действует") {
		t.Errorf("spent: %q", m[0].Text)
	}
	// Revoke from the bot.
	owner.handle(press(5, 42, "inv:never", time.Now()))
	code := s.listInvites(time.Now())[0].Code
	owner.handle(press(6, 42, "inv-:"+code, time.Now()))
	if len(s.listInvites(time.Now())) != 0 || !strings.Contains(f.lastEditText(), "отозвано") {
		t.Errorf("revoke:\n%s", f.lastEditText())
	}
}

func TestInviteOnThePanelAndTheCLI(t *testing.T) {
	wt := newWebTest(t, "192.168.1.50")
	if rec := wt.do("POST", "/invites/new", url.Values{"how": {"+90"}, "note": {"Oleg"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("new: %d %s", rec.Code, rec.Body)
	}
	invs := wt.s.listInvites(time.Now())
	if len(invs) != 1 || invs[0].How != "+90" {
		t.Fatalf("invites %+v", invs)
	}
	body := wt.do("GET", "/requests", nil).Body.String()
	if !strings.Contains(body, "<code>"+invs[0].Code+"</code>") || !strings.Contains(body, "«Oleg»") {
		t.Errorf("requests page lacks the invite")
	}
	wt.do("POST", "/invites/"+invs[0].Code, url.Values{})
	if len(wt.s.listInvites(time.Now())) != 0 {
		t.Error("not revoked")
	}
	var out strings.Builder
	if err := run([]string{"invite", "--expires", "30", "--note", "Ann"}, nil, &out); err != nil || !strings.Contains(out.String(), "access +30") {
		t.Errorf("cli: %v\n%s", err, out.String())
	}
	out.Reset()
	run([]string{"invite", "list"}, nil, &out)
	if !strings.Contains(out.String(), "Ann") {
		t.Errorf("list:\n%s", out.String())
	}
}
