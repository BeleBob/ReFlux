package mobile

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/transport/control"
	"github.com/p1neappleXpress/OpenFlux/transport/manager"
)

type fakeCaptchaTransport struct {
	transport.Transport
	notify  func(err error, name, url, html, reason string)
	applied chan map[string]string
}

func (f *fakeCaptchaTransport) SetErrorNotifier(fn func(err error, name, url, html, reason string)) {
	f.notify = fn
}
func (f *fakeCaptchaTransport) FetchCookies() (map[string]string, error) { return nil, nil }
func (f *fakeCaptchaTransport) ApplyCookies(jar map[string]string) error {
	f.applied <- jar
	return nil
}

func TestCaptchaFlow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookies.json")
	if msg := SetCookieStorePath(path); msg != "" {
		t.Fatal(msg)
	}
	raw := &fakeCaptchaTransport{applied: make(chan map[string]string, 1)}
	attachCaptcha("yandex", "https://docs.example/d", raw)

	raw.notify(errors.New("captcha"), "yandex", "https://docs.example/d", "", "smartcaptcha")
	if PendingCaptchaURL() != "https://docs.example/d" || PendingCaptchaReason() != "smartcaptcha" {
		t.Fatalf("pending = %q/%q", PendingCaptchaURL(), PendingCaptchaReason())
	}

	if msg := SubmitCaptchaCookies("spravka=abc; yandexuid=42;bad; =x"); msg != "" {
		t.Fatal(msg)
	}
	if PendingCaptchaURL() != "" {
		t.Fatal("pending not cleared after submit")
	}
	select {
	case jar := <-raw.applied:
		if len(jar) != 2 || jar["spravka"] != "abc" || jar["yandexuid"] != "42" {
			t.Fatalf("applied jar = %v", jar)
		}
	case <-time.After(time.Second):
		t.Fatal("cookies not applied to live transport")
	}

	// A fresh start with the same transport/URL replays the saved cookies.
	next := &fakeCaptchaTransport{applied: make(chan map[string]string, 1)}
	SetCookieStorePath(path)
	attachCaptcha("yandex", "https://docs.example/d", next)
	select {
	case jar := <-next.applied:
		if jar["spravka"] != "abc" {
			t.Fatalf("replayed jar = %v", jar)
		}
	default:
		t.Fatal("saved cookies not replayed on next start")
	}

	// After detach (stopped / failed start) cookies are only saved.
	detachCaptcha()
	SubmitCaptchaCookies("spravka=new")
	select {
	case jar := <-next.applied:
		t.Fatalf("applied to detached transport: %v", jar)
	case <-time.After(50 * time.Millisecond):
	}
}

// The core, not the app, says which pages are a script's own.
func TestOwnPageFlag(t *testing.T) {
	raw := &fakeCaptchaTransport{applied: make(chan map[string]string, 1)}
	attachCaptcha("script", "local://x", raw)
	t.Cleanup(detachCaptcha)

	for _, c := range []struct {
		url, html string
		own       bool
	}{
		{"https://docs.example/d", "", false},
		{"http://127.0.0.1:41234/", "", true},
		{"", "<p>setup</p>", true},
	} {
		raw.notify(errors.New("setup"), "script", c.url, c.html, "login")
		if PendingCaptchaOwn() != c.own {
			t.Errorf("url %q html %q: own = %v, want %v", c.url, c.html, PendingCaptchaOwn(), c.own)
		}
		if PendingCaptchaURL() != c.url || PendingCaptchaHTML() != c.html {
			t.Errorf("pending = %q / %q, want %q / %q", PendingCaptchaURL(), PendingCaptchaHTML(), c.url, c.html)
		}
		CancelCaptcha()
		if PendingCaptchaOwn() {
			t.Errorf("own still set after CancelCaptcha (url %q)", c.url)
		}
	}
}

func TestSubmitCaptchaDataReadsTheScopedPayload(t *testing.T) {
	raw := &fakeCaptchaTransport{applied: make(chan map[string]string, 1)}
	attachCaptcha("script", "local://x", raw)
	t.Cleanup(detachCaptcha)
	raw.notify(errors.New("setup"), "script", "", "<p>setup</p>", "")

	if msg := SubmitCaptchaData(`{"client":{"token":"t","port":8080,"on":true,"x":{"y":1}},"node":{"n":"1"}}`); msg != "" {
		t.Fatal(msg)
	}
	select {
	case jar := <-raw.applied:
		if len(jar) != 3 || jar["token"] != "t" || jar["port"] != "8080" || jar["on"] != "true" {
			t.Fatalf("applied jar = %v", jar)
		}
	case <-time.After(time.Second):
		t.Fatal("data not applied to the live transport")
	}
	if PendingCaptchaHTML() != "" || PendingCaptchaOwn() {
		t.Fatal("pending page not cleared after submit")
	}
	if msg := SubmitCaptchaData(`{"o":{}}`); msg == "" {
		t.Fatal("a submission with no values must be refused")
	}
	if msg := SubmitCaptchaData(`oops`); msg == "" {
		t.Fatal("a submission that is not JSON must be refused")
	}
}

// One mail.ru sign-in serves every mail.ru document of a channel: the
// cookies the person hands over for the document that asked reach the
// others too, now and at the next start, and no other transport's.
func TestMailruLoginServesEveryDocument(t *testing.T) {
	dir := t.TempDir()
	if err := SetCookieStorePath(filepath.Join(dir, "cookies.json")); err != "" {
		t.Fatal(err)
	}
	t.Cleanup(func() { SetCookieStorePath("") })
	sess, err := transport.NewSession(transport.PeerParameters{
		Capabilities: control.CapabilityIPv4 | control.CapabilityTCP, MaxPacketSize: 1500,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Stop() })
	m := manager.New(sess, nil, testSecret, "ctx")
	carriers := map[string]*stubCarrier{"mailru": {}, "mailru-2": {}, "yandex": {}}
	keys := map[string]string{
		"mailru":   "mailru https://cloud.mail.ru/public/a/1",
		"mailru-2": "mailru https://cloud.mail.ru/public/b/2",
		"yandex":   "yandex https://docs.yandex.ru/x",
	}
	for name, c := range carriers {
		if err := m.Add(name, strings.SplitN(keys[name], " ", 2)[0], c, 100, c); err != nil {
			t.Fatal(err)
		}
	}
	attachSessionCaptcha(m, keys, nil)
	t.Cleanup(CancelCaptcha)

	m.NotifyCaptcha("mailru-2", "https://account.mail.ru/login?page=x", "", "login")
	if PendingCaptchaReason() != "login" {
		t.Fatalf("pending reason %q", PendingCaptchaReason())
	}
	if msg := SubmitCaptchaCookies("Mpop=m; sdcs=s"); msg != "" {
		t.Fatal(msg)
	}
	deadline := time.Now().Add(3 * time.Second)
	for carriers["mailru"].applied()["Mpop"] != "m" || carriers["mailru-2"].applied()["Mpop"] != "m" {
		if time.Now().After(deadline) {
			t.Fatalf("applied: mailru %v, mailru-2 %v", carriers["mailru"].applied(), carriers["mailru-2"].applied())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if carriers["yandex"].applied() != nil {
		t.Errorf("the mail.ru login went to yandex: %v", carriers["yandex"].applied())
	}
	store, _ := transport.NewCookieStore(filepath.Join(dir, "cookies.json"))
	for _, k := range []string{keys["mailru"], keys["mailru-2"]} {
		if store.Load(k)["sdcs"] != "s" {
			t.Errorf("not kept for the next start: %s", k)
		}
	}
	if store.Load(keys["yandex"]) != nil {
		t.Error("kept for yandex")
	}
}
