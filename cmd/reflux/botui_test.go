package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/telegram"
)

var fileExt = map[string]bool{"conf": true, "yml": true, "json": true, "sock": true, "service": true, "log": true, "stat": true}

var verbRe = regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z]`)

// Both languages of a message take the same arguments.
func TestMessagesHaveBothLanguages(t *testing.T) {
	for id, m := range messages {
		if m[0] == "" || m[1] == "" {
			t.Errorf("%s: missing a language: %q", id, m)
			continue
		}
		en, ru := verbRe.FindAllString(strings.ReplaceAll(m[0], "%%", ""), -1), verbRe.FindAllString(strings.ReplaceAll(m[1], "%%", ""), -1)
		if strings.Join(en, " ") != strings.Join(ru, " ") {
			t.Errorf("%s: arguments differ: en %v, ru %v", id, en, ru)
		}
	}
}

// Every message id in the code is in the catalog: tr prints a missing id
// as is, which a test of the screens would not always catch.
func TestEveryMessageIDExists(t *testing.T) {
	prefixes := map[string]bool{}
	for id := range messages {
		p, _, _ := strings.Cut(id, ".")
		prefixes[p] = true
	}
	idRe := regexp.MustCompile(`"([a-z]+)\.([a-z0-9.!]+)"`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "i18n.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range idRe.FindAllStringSubmatch(string(b), -1) {
			id := m[1] + "." + m[2]
			if fileExt[m[2]] {
				continue // node.conf, compose.yml
			}
			if prefixes[m[1]] && !strings.HasSuffix(id, ".") {
				if _, ok := messages[id]; !ok {
					t.Errorf("%s: message %q is not in the catalog", f, id)
				}
			}
		}
	}
	for _, c := range botCommands {
		if _, ok := messages["cmd."+c]; !ok {
			t.Errorf("command %s has no description", c)
		}
	}
}

func TestBotLanguageIsSavedAndUsed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	s := Store{Root: home}
	if err := s.saveBotConfig(botConfig{Token: testToken, Chat: 42}); err != nil {
		t.Fatal(err)
	}
	f := newFakeTG(t)
	c, _ := s.loadBotConfig()
	b := newBot(s, c)
	if b.lang != langRU {
		t.Fatalf("default language %q, want ru", b.lang)
	}
	b.handle(press(1, 42, "lang:en", time.Now()))
	if c, _ := s.loadBotConfig(); c.Lang != "en" || c.Token != testToken || c.Chat != 42 {
		t.Errorf("config after switching = %+v", c)
	}
	if len(f.edited) != 1 || !strings.Contains(f.edited[0], "Language: English") {
		t.Errorf("settings screen = %q", f.edited)
	}
	if !strings.Contains(strings.Join(f.methods, " "), "setMyCommands") {
		t.Error("the command menu was not translated")
	}
	b.handle(msg(2, 42, "/start"))
	if m := f.messages(); !strings.Contains(m[len(m)-1].Buttons.String(), "Refresh") {
		t.Errorf("home after switching: %+v", m[len(m)-1])
	}
}

func TestBotAddsAClientFromTheNextMessage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	s := Store{Root: home}
	f := newFakeTG(t)
	b := newBot(s, botConfig{Token: testToken, Chat: 42, Lang: "en"})

	b.handle(msg(1, 42, "friend "+testURL)) // not asked for: shows home
	if _, err := s.Get("friend"); err == nil {
		t.Fatal("a plain message added a client")
	}
	b.handle(press(2, 42, "add", time.Now()))
	b.handle(msg(3, 42, "friend "+testURL+" 2w"))
	c, err := s.Get("friend")
	if err != nil || c.Expires.IsZero() {
		t.Fatalf("client %+v, %v", c, err)
	}
	m := f.messages()
	if last := m[len(m)-1]; !strings.Contains(last.Text, "✅ Client <b>friend</b> added") || !strings.Contains(last.Buttons.String(), "qr:friend") {
		t.Errorf("reply = %+v", last)
	}
	b.handle(msg(4, 42, "other "+testURL+"x")) // the prompt was used up
	if _, err := s.Get("other"); err == nil {
		t.Error("a second plain message added a client")
	}
}

func TestAccessScreenSetsTheExpiry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	calls := fakeDocker(t, "")
	s := Store{Root: home}
	f := newFakeTG(t)
	b := newBot(s, botConfig{Token: testToken, Chat: 42, Lang: "en"})
	if _, err := s.Add("guest", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	expires := func() time.Time { c, _ := s.Get("guest"); return c.Expires }

	b.handle(press(1, 42, "acc:guest", time.Now()))
	if len(f.edited) != 1 || !strings.Contains(f.edited[0], "access expiry") {
		t.Fatalf("access screen = %q", f.edited)
	}
	// +days on unlimited access count from now.
	b.handle(press(2, 42, "ax:guest:+7", time.Now()))
	if d := time.Until(expires()); d < 7*24*time.Hour-time.Minute || d > 7*24*time.Hour {
		t.Errorf("+7 on unlimited access: %v left", d)
	}
	// While it lasts, from its end.
	end := expires()
	b.handle(press(3, 42, "ax:guest:+30", time.Now()))
	if !expires().Equal(end.AddDate(0, 0, 30)) {
		t.Errorf("+30 = %v, want %v", expires(), end.AddDate(0, 0, 30))
	}
	b.handle(press(4, 42, "ax:guest:never", time.Now()))
	if !expires().IsZero() {
		t.Errorf("never left %v", expires())
	}
	// Ending now takes the node away at once.
	*calls = nil
	b.handle(press(5, 42, "ax:guest:now", time.Now()))
	if c, _ := s.Get("guest"); c.Active(time.Now()) {
		t.Error("still active after ending now")
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "up --detach --remove-orphans") {
		t.Errorf("ending now did not apply: %q", *calls)
	}
	// A typed date, asked for by the button.
	b.handle(press(6, 42, "axin:guest", time.Now()))
	b.handle(msg(7, 42, "2030-01-15"))
	want := time.Date(2030, 1, 16, 0, 0, 0, 0, time.Local)
	if !expires().Equal(want) {
		t.Errorf("typed date: %v, want %v", expires(), want)
	}
	b.handle(press(8, 42, "ax:guest:+99999", time.Now()))
	if !expires().Equal(want) {
		t.Error("an absurd extension was applied")
	}
}

func TestLinkAClientToTheOwner(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	s := Store{Root: home}
	f := newFakeTG(t)
	b := newBot(s, botConfig{Token: testToken, Chat: 42, Lang: "ru"})
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	up := press(1, 42, "tgme:phone", time.Now())
	up.Callback.From = &telegram.User{ID: 42, FirstName: "Дмитрий", Username: "dima"}
	b.handle(up)
	c, _ := s.Get("phone")
	if c.Telegram == nil || c.Telegram.ID != 42 || c.Telegram.String() != "Дмитрий (@dima)" {
		t.Fatalf("linked %+v", c.Telegram)
	}
	if len(f.edited) != 1 || !strings.Contains(f.edited[0], "Telegram: Дмитрий (@dima)") {
		t.Errorf("card = %q", f.edited)
	}
	if home := b.home().text; !strings.Contains(home, "<b>phone</b> (Дмитрий)") {
		t.Errorf("home does not name the owner:\n%s", home)
	}
	b.handle(press(2, 42, "tgoff:phone", time.Now()))
	if c, _ := s.Get("phone"); c.Telegram != nil {
		t.Error("not unlinked")
	}
}

func TestRenameFromTheBot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	s := Store{Root: home}
	f := newFakeTG(t)
	b := newBot(s, botConfig{Token: testToken, Chat: 42, Lang: "en"})
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	b.handle(press(1, 42, "ren:phone", time.Now()))
	b.handle(msg(2, 42, "dima"))
	if _, err := s.Get("dima"); err != nil {
		t.Fatalf("not renamed: %v", err)
	}
	if m := f.messages(); !strings.Contains(m[len(m)-1].Text, "<b>phone</b> is now <b>dima</b>") {
		t.Errorf("reply %+v", m[len(m)-1])
	}
}

func TestMutedAlertsAreNotSent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	s := Store{Root: home}
	if err := s.saveBotConfig(botConfig{Token: testToken, Chat: 42}); err != nil {
		t.Fatal(err)
	}
	newFakeTG(t)
	c, _ := s.loadBotConfig()
	b := newBot(s, c)
	b.handle(press(1, 42, "mute:updates", time.Now()))
	b.handle(press(2, 42, "mute:server", time.Now()))
	b.handle(press(3, 42, "mute:server", time.Now())) // and back on
	if c, _ := s.loadBotConfig(); strings.Join(c.Mute, ",") != "updates" {
		t.Fatalf("saved mute = %v", c.Mute)
	}
	b.handle(press(4, 42, "mute:bogus", time.Now()))
	if c, _ := s.loadBotConfig(); strings.Join(c.Mute, ",") != "updates" {
		t.Errorf("an unknown category was saved: %v", c.Mute)
	}
	if news := b.check(true); len(news) != 0 {
		t.Errorf("start summary sent with updates muted: %q", news)
	}
	for key, want := range map[string]string{"world": "tunnels", "russia": "tunnels", "node:phone": "nodes",
		"doc:phone": "nodes", "image:x": "updates", "disk:/": "server", "cron": "server"} {
		if got := category(key); got != want {
			t.Errorf("category(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestBotServerScreenAndNodeRestart(t *testing.T) {
	proc, sys := fakeHostTree(t)
	writeFile(t, proc+"/stat", "cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 0 0 0\ncpu1 1 0 0 0\n")
	writeFile(t, proc+"/meminfo", "MemTotal: 8000000 kB\nMemAvailable: 6000000 kB\n")
	writeFile(t, proc+"/loadavg", "1.45 1.12 1.09 1/310 1\n")
	writeFile(t, proc+"/uptime", "90000 1\n")
	writeFile(t, sys+"/class/hwmon/hwmon1/name", "coretemp\n")
	writeFile(t, sys+"/class/hwmon/hwmon1/temp1_input", "41000\n")
	old := measureWindow
	measureWindow = 10 * time.Millisecond
	defer func() { measureWindow = old }()

	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	calls := fakeDocker(t, "")
	s := Store{Root: home}
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	f := newFakeTG(t)
	b := newBot(s, botConfig{Token: testToken, Chat: 42, Lang: "ru"})
	b.handle(press(1, 42, "srv", time.Now()))
	for _, want := range []string{"Процессор: ", "ядер 2", "Память: 25%", "Температура: 41 °C", "Работает 25 ч"} {
		if !strings.Contains(f.edited[0], want) {
			t.Errorf("server screen lacks %q:\n%s", want, f.edited[0])
		}
	}
	*calls = nil
	b.handle(press(2, 42, "rsn:phone", time.Now()))
	if !strings.Contains(strings.Join(*calls, "\n"), "up --detach --no-deps --force-recreate node-phone") {
		t.Errorf("node restart ran %q", *calls)
	}
}
