package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var fileExt = map[string]bool{"conf": true, "yml": true, "json": true, "sock": true, "service": true, "log": true}

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

func TestPlus30DaysExtendsFromTheEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	s := Store{Root: home}
	newFakeTG(t)
	b := newBot(s, botConfig{Token: testToken, Chat: 42, Lang: "en"})
	if _, err := s.Add("guest", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	c, _ := s.Get("guest")
	end := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second)
	c.Expires = end
	s.Save(c)
	b.handle(press(1, 42, "ex30:guest", time.Now()))
	if c, _ := s.Get("guest"); !c.Expires.Equal(end.AddDate(0, 0, 30)) {
		t.Errorf("expires %v, want %v", c.Expires, end.AddDate(0, 0, 30))
	}
	b.handle(press(2, 42, "exn:guest", time.Now()))
	if c, _ := s.Get("guest"); !c.Expires.IsZero() {
		t.Errorf("♾ left expiry %v", c.Expires)
	}
	b.handle(press(3, 42, "ex30:guest", time.Now()))
	if c, _ := s.Get("guest"); !c.Expires.IsZero() {
		t.Errorf("+30 days limited unlimited access: %v", c.Expires)
	}
}
