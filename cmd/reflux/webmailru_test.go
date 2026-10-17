package main

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

// The panel takes the nodes' mail.ru cookies, checks them and never shows
// them; a login mail.ru refuses is turned down with mail.ru's reason.
func TestWebMailruLogin(t *testing.T) {
	wt := newWebTest(t)
	wt.signIn()
	fakeLoginCheck(t)
	if body := wt.do("GET", "/mailru", nil).Body.String(); !strings.Contains(body, "Mpop=") || !strings.Contains(body, `action="/mailru/login"`) {
		t.Fatalf("the page has no form:\n%s", body)
	}
	rec := wt.do("POST", "/mailru/login", url.Values{"cookies": {"Mpop=1:2:owner@mail.ru:; sdcs=S1SECRET"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/mailru?ok=mailru" {
		t.Fatalf("login: %d %s\n%s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if b, _ := os.ReadFile(wt.s.mailruCookiesPath()); !strings.Contains(string(b), "sdcs=S1SECRET") {
		t.Fatalf("cookies not saved: %q", b)
	}
	body := wt.do("GET", "/mailru?ok=mailru", nil).Body.String()
	if !strings.Contains(body, "owner@mail.ru") || strings.Contains(body, "S1SECRET") {
		t.Errorf("the page after saving:\n%s", body)
	}
	if rec := wt.do("POST", "/mailru/check", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Errorf("check: %d\n%s", rec.Code, rec.Body)
	}
	rec = wt.do("POST", "/mailru/login", url.Values{"cookies": {"Mpop=1:2:owner@mail.ru:; sdcs=OLDSECRET"}})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "403 user") || strings.Contains(rec.Body.String(), "OLDSECRET") {
		t.Errorf("refused login: %d\n%s", rec.Code, rec.Body)
	}
}
