package manager

import (
	"path/filepath"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/control"
)

// An exit signed in with its owner's Yandex account must not hand that
// login to clients: everyone with the channel key can ask for the jar.
func TestShareableCookiesDropAccountLogin(t *testing.T) {
	jar := map[string]string{
		"Session_id": "s", "sessionid2": "s2", "sessar": "a", "L": "l",
		"yandex_login": "me", "spravka": "captcha-pass", "yandexuid": "u",
	}
	got := shareableCookies("vyandex", jar)
	for _, name := range []string{"Session_id", "sessionid2", "sessar", "L", "yandex_login"} {
		if _, ok := got[name]; ok {
			t.Fatalf("%s must not be shared: %v", name, got)
		}
	}
	if got["spravka"] != "captcha-pass" || got["yandexuid"] != "u" {
		t.Fatalf("non-account cookies must still be shared: %v", got)
	}
	if jar["Session_id"] != "s" {
		t.Fatal("the source jar must not be modified")
	}
}

// A mail.ru exit signed in with its owner's account hands clients none of
// its jar: every cookie in it is that account's (Mpop is the mail.ru login,
// sdcs the cloud session), and a client that applied them would open its
// documents as the owner.
func TestShareableCookiesKeepMailruAccount(t *testing.T) {
	if got := shareableCookies("mailru", map[string]string{"Mpop": "m", "sdcs": "s", "t": "x"}); len(got) != 0 {
		t.Fatalf("shared %v", got)
	}
	m := newTestManager(t)
	if err := m.Add("mailru-2", "mailru", &fakeTransport{}, 100, &fakeCookieProvider{}); err != nil {
		t.Fatal(err)
	}
	if typ := m.entryType("mailru-2"); typ != "mailru" {
		t.Fatalf("entryType = %q: a request for its jar would be answered as another type", typ)
	}
}

// Cookies a client sends after passing a check add to what the exit keeps
// (its account login) instead of replacing it.
func TestAcceptCookiesMergesWithStored(t *testing.T) {
	m := newTestManager(t)
	p := &fakeCookieProvider{}
	if err := m.Add("vyandex", "vyandex", &fakeTransport{}, 100, p); err != nil {
		t.Fatal(err)
	}
	store, err := transport.NewCookieStore(filepath.Join(t.TempDir(), "cookies.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("doc", map[string]string{"Session_id": "login", "spravka": "old"}); err != nil {
		t.Fatal(err)
	}
	if err := m.UseCookieStore(store, "vyandex", "doc"); err != nil {
		t.Fatal(err)
	}
	m.DispatchControl(control.SubtypeCookiesOffer, offer(t, "vyandex", map[string]string{"spravka": "new"}))
	jar, _ := p.FetchCookies()
	if jar["Session_id"] != "login" || jar["spravka"] != "new" {
		t.Fatalf("live jar = %v", jar)
	}
	if saved := store.Load("doc"); saved["Session_id"] != "login" || saved["spravka"] != "new" {
		t.Fatalf("stored jar = %v", saved)
	}
}
