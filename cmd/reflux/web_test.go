package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type webTest struct {
	t   *testing.T
	s   Store
	h   http.Handler
	w   *webServer
	jar []*http.Cookie
}

func webOf(t *testing.T, wt *webTest) *webServer { return wt.w }

func newWebTest(t *testing.T, trusted ...string) *webTest {
	t.Helper()
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	s := Store{Root: home}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	cfg := webConfig{Listen: "192.168.1.201:8686", Trusted: trusted}
	if err := s.saveWebConfig(cfg); err != nil {
		t.Fatal(err)
	}
	w, err := newWebServer(s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &webTest{t: t, s: s, h: w.routes(), w: w}
}

// do sends a request from addr (a LAN machine by default) with the
// cookies collected so far; form makes it a same-origin POST.
func (wt *webTest) do(method, path string, form url.Values, addr ...string) *httptest.ResponseRecorder {
	wt.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, "http://192.168.1.201:8686"+path, body)
	r.RemoteAddr = "192.168.1.50:40000"
	if len(addr) > 0 {
		r.RemoteAddr = addr[0]
	}
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "http://192.168.1.201:8686")
	}
	for _, c := range wt.jar {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	wt.h.ServeHTTP(rec, r)
	wt.jar = append(wt.jar, rec.Result().Cookies()...)
	return rec
}

func (wt *webTest) signIn() {
	wt.t.Helper()
	link, err := wt.s.loginLink()
	if err != nil {
		wt.t.Fatal(err)
	}
	u, _ := url.Parse(link)
	if rec := wt.do("GET", "/login?"+u.RawQuery, nil); rec.Code != http.StatusSeeOther {
		wt.t.Fatalf("login: %d", rec.Code)
	}
}

func TestWebSignInWithAOneTimeLink(t *testing.T) {
	wt := newWebTest(t)
	if rec := wt.do("GET", "/", nil); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "/web") {
		t.Fatalf("signed out: %d\n%s", rec.Code, rec.Body)
	}
	link, err := wt.s.loginLink()
	if err != nil || !strings.HasPrefix(link, "http://192.168.1.201:8686/login?t=") {
		t.Fatalf("link %q, %v", link, err)
	}
	u, _ := url.Parse(link)
	if rec := wt.do("GET", "/login?"+u.RawQuery, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("login: %d", rec.Code)
	}
	c := wt.jar[len(wt.jar)-1]
	if c.Name != sessionCookie || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie %+v", c)
	}
	if rec := wt.do("GET", "/", nil); rec.Code != http.StatusOK {
		t.Fatalf("signed in: %d", rec.Code)
	}
	// The link works once.
	other := &webTest{t: t, s: wt.s, h: wt.h}
	if rec := other.do("GET", "/login?"+u.RawQuery, nil); rec.Code != http.StatusForbidden {
		t.Errorf("second use of the link: %d", rec.Code)
	}
	// Signing out ends the session.
	wt.do("POST", "/logout", url.Values{})
	stale := &webTest{t: t, s: wt.s, h: wt.h, jar: []*http.Cookie{c}}
	if rec := stale.do("GET", "/", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("after signing out: %d", rec.Code)
	}
}

func TestWebLoginLinksExpire(t *testing.T) {
	wt := newWebTest(t)
	link, _ := wt.s.loginLink()
	u, _ := url.Parse(link)
	token := u.Query().Get("t")
	wt.s.withTokens("login", func(set expiring) { set[tokenHash(token)] = time.Now().Add(-time.Second) })
	if rec := wt.do("GET", "/login?t="+token, nil); rec.Code != http.StatusForbidden {
		t.Errorf("expired link: %d", rec.Code)
	}
}

func TestWebAnswersTheHomeNetworkOnly(t *testing.T) {
	wt := newWebTest(t, "192.168.1.124")
	if rec := wt.do("GET", "/", nil, "8.8.8.8:5000"); rec.Code != http.StatusForbidden {
		t.Errorf("public address: %d", rec.Code)
	}
	if rec := wt.do("GET", "/", nil, "192.168.1.124:5000"); rec.Code != http.StatusOK {
		t.Errorf("trusted address: %d", rec.Code)
	}
	if rec := wt.do("GET", "/", nil, "192.168.1.125:5000"); rec.Code != http.StatusUnauthorized {
		t.Errorf("other LAN address without a session: %d", rec.Code)
	}
}

func TestWebActionsComeFromItsOwnPages(t *testing.T) {
	wt := newWebTest(t, "192.168.1.50")
	if _, err := wt.s.Add("guest", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	// A cross-site form: no Origin of ours.
	r := httptest.NewRequest("POST", "http://192.168.1.201:8686/c/guest/pause", strings.NewReader(""))
	r.RemoteAddr = "192.168.1.50:1"
	r.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	wt.h.ServeHTTP(rec, r)
	if c, _ := wt.s.Get("guest"); rec.Code != http.StatusForbidden || c.Paused {
		t.Fatalf("cross-site pause: %d, paused=%v", rec.Code, c.Paused)
	}
	if rec := wt.do("POST", "/c/guest/pause", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("pause: %d %s", rec.Code, rec.Body)
	}
	if c, _ := wt.s.Get("guest"); !c.Paused {
		t.Error("not paused")
	}
	wt.do("POST", "/c/guest/resume", url.Values{})
	wt.do("POST", "/c/guest/expire", url.Values{"how": {"+7"}})
	if c, _ := wt.s.Get("guest"); c.Paused || c.Expires.IsZero() {
		t.Errorf("after resume and +7: %+v", c)
	}
	wt.do("POST", "/c/guest/expire", url.Values{"how": {"date"}, "date": {"2030-01-15"}})
	if c, _ := wt.s.Get("guest"); !c.Expires.Equal(time.Date(2030, 1, 16, 0, 0, 0, 0, time.Local)) {
		t.Errorf("date: %v", c.Expires)
	}
	if rec := wt.do("POST", "/c/guest/revoke", url.Values{"confirm": {"nope"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("revoke without the name typed: %d", rec.Code)
	}
	if _, err := wt.s.Get("guest"); err != nil {
		t.Fatal("revoked without the name typed")
	}
	if rec := wt.do("POST", "/c/guest/revoke", url.Values{"confirm": {"guest"}}); rec.Code != http.StatusSeeOther {
		t.Errorf("revoke: %d", rec.Code)
	}
	if _, err := wt.s.Get("guest"); err == nil {
		t.Error("not revoked")
	}
}

func TestWebAddAndShow(t *testing.T) {
	wt := newWebTest(t)
	wt.signIn()
	rec := wt.do("POST", "/add", url.Values{"name": {"friend"}, "url": {"https://disk.yandex.ru/i/abc"}, "expires": {"30d"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/c/friend?ok=added" {
		t.Fatalf("add: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	c, err := wt.s.Get("friend")
	if err != nil || c.Transport != "yandex" || c.Expires.IsZero() {
		t.Fatalf("added %+v, %v", c, err)
	}
	rec = wt.do("POST", "/c/friend/show", url.Values{})
	key, _ := wt.s.Key("friend")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, key) || !strings.Contains(body, "data:image/png;base64,") ||
		!strings.Contains(body, "openflux://") {
		t.Fatalf("show: %d", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("the access data may be cached")
	}
	if strings.Contains(wt.do("GET", "/c/friend", nil).Body.String(), key) {
		t.Error("the client page shows the key without asking")
	}
}

var rawIDRe = regexp.MustCompile(`\b(web|ui|b|gw|short|cmd|alerts)\.[a-z0-9.]+\b`)

// Every page renders, in both languages, with no message id left raw.
func TestWebPagesRender(t *testing.T) {
	for _, l := range []string{"ru", "en"} {
		wt := newWebTest(t, "192.168.1.50")
		wt.s.saveBotConfig(botConfig{Token: testToken, Chat: 42, Lang: l})
		if _, err := wt.s.Add("phone", "mailru", testURL); err != nil {
			t.Fatal(err)
		}
		// Figures for the tiles and charts: two samples of a fake host.
		proc, sys := fakeHostTree(t)
		writeFile(t, proc+"/stat", "cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 0 0 0\n")
		writeFile(t, proc+"/meminfo", "MemTotal: 8000000 kB\nMemAvailable: 6000000 kB\nSwapTotal: 1000 kB\nSwapFree: 500 kB\n")
		writeFile(t, proc+"/loadavg", "1.45 1.12 1.09 1/310 1\n")
		writeFile(t, proc+"/uptime", "90000 1\n")
		writeFile(t, sys+"/class/hwmon/hwmon1/name", "coretemp\n")
		writeFile(t, sys+"/class/hwmon/hwmon1/temp1_input", "41000\n")
		writeFile(t, sys+"/class/hwmon/hwmon1/temp1_label", "Package id 0\n")
		w := webOf(t, wt)
		// A running egress, so the pages show the tunnels too.
		inner := runDocker
		runDocker = func(stdout io.Writer, args ...string) error {
			if strings.Join(args, " ") == "exec reflux-egress cat /run/reflux-egress/status.json" {
				io.WriteString(stdout, `{"world":"world-3.conf","world_ok":true,"world_selected":"world-3.conf","ru_ok":true,`+
					`"ru_mode":"tunnel","ru_fallback":true,"carrier_direct":true,"carrier_addrs":["95.163.59.187"],"killswitch_dropped":0}`)
				return nil
			}
			return inner(stdout, args...)
		}
		w.stats.tick(time.Now().Add(-10 * time.Second))
		writeFile(t, proc+"/stat", "cpu  200 0 100 800 100 0 0 0 0 0\ncpu0 1 0 0 0\n")
		w.stats.tick(time.Now().Add(-5 * time.Second))
		writeFile(t, proc+"/stat", "cpu  500 0 100 900 100 0 0 0 0 0\ncpu0 1 0 0 0\n")
		w.stats.tick(time.Now())
		wt.s.logEvents([]event{{At: time.Now(), Level: "FAIL", RU: "мир не работает", EN: "world is down"}})
		for _, p := range []string{"/", "/server", "/events", "/c/phone", "/add", "/doctor", "/gateway", "/speed", "/logs/phone", "/logs/egress", "/?ok=added", "/c/phone?ok=restarted_node"} {
			rec := wt.do("GET", p, nil)
			if rec.Code != http.StatusOK {
				t.Errorf("%s %s: %d\n%s", l, p, rec.Code, rec.Body)
				continue
			}
			body := rec.Body.String()
			if m := rawIDRe.FindString(body); m != "" {
				t.Errorf("%s %s shows the raw message id %q", l, p, m)
			}
			if strings.Contains(body, "%!") {
				t.Errorf("%s %s has a formatting error:\n%s", l, p, body)
			}
		}
		if body := wt.do("GET", "/server", nil).Body.String(); !strings.Contains(body, "75%") || !strings.Contains(body, "41 °C") ||
			!strings.Contains(body, "<polyline") {
			t.Errorf("%s: server page lacks the figures:\n%s", l, body)
		}
		if body := wt.do("GET", "/", nil).Body.String(); !strings.Contains(body, map[string]string{"ru": "мир не работает", "en": "world is down"}[l]) {
			t.Errorf("%s: home lacks the latest event", l)
		}
		if rec := wt.do("GET", "/c/nobody", nil); !strings.Contains(rec.Body.String(), "nobody") {
			t.Errorf("unknown client page: %d", rec.Code)
		}
	}
}

func TestExpiryFromTheForm(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)
	later := now.AddDate(0, 0, 10)
	for _, c := range []struct {
		how, date string
		cur, want time.Time
	}{
		{"+7", "", time.Time{}, now.AddDate(0, 0, 7)},
		{"+30", "", later, later.AddDate(0, 0, 30)},
		{"+1", "", now.AddDate(0, 0, -3), now.AddDate(0, 0, 1)},
		{"never", "", later, time.Time{}},
		{"now", "", later, now},
		{"date", "2026-12-31", later, time.Date(2027, 1, 1, 0, 0, 0, 0, time.Local)},
	} {
		got, err := expiryFrom(c.how, c.date, c.cur, now)
		if err != nil || !got.Equal(c.want) {
			t.Errorf("%s %s: %v, %v; want %v", c.how, c.date, got, err, c.want)
		}
	}
	for _, bad := range []string{"+0", "+99999", "soon", ""} {
		if _, err := expiryFrom(bad, "", time.Time{}, now); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTransportFromTheLink(t *testing.T) {
	for u, want := range map[string]string{
		"https://cloud.mail.ru/public/Hw4N/auCL457qg": "mailru",
		"https://disk.yandex.ru/i/HU63imh0dOiDRw":     "yandex",
		"https://yandex.ru/whiteboard/?hash=abc":      "boards",
		"https://docs.yandex.ru/edit/d/xyz?from=1":    "yandex",
	} {
		if got := transportOf(u); got != want {
			t.Errorf("%s: %s, want %s", u, got, want)
		}
	}
}

func TestWebInstall(t *testing.T) {
	home := t.TempDir()
	s := Store{Root: filepath.Join(home, "reflux")}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	var calls []string
	old := runCmd
	runCmd = func(stdout io.Writer, name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}
	defer func() { runCmd = old }()
	if err := webInstall(s, "0.0.0.0:8686", "", io.Discard); err == nil {
		t.Error("listening on every address accepted")
	}
	if err := webInstall(s, "192.168.1.201:8686", "192.168.1.124, 8.8.8.8", io.Discard); err == nil {
		t.Error("a public trusted address accepted")
	}
	var out strings.Builder
	if err := webInstall(s, "192.168.1.201:8686", "192.168.1.124,192.168.1.140", &out); err != nil {
		t.Fatal(err)
	}
	c, err := s.loadWebConfig()
	if err != nil || c.Listen != "192.168.1.201:8686" || strings.Join(c.Trusted, ",") != "192.168.1.124,192.168.1.140" {
		t.Fatalf("config %+v, %v", c, err)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "systemctl --user restart reflux-web.service") {
		t.Errorf("calls %q", calls)
	}
	if !strings.Contains(out.String(), "sudo ufw allow from 192.168.1.0/24 to any port 8686 proto tcp") {
		t.Errorf("no firewall hint:\n%s", out.String())
	}
}
