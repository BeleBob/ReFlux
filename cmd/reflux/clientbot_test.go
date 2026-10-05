package main

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/telegram"
)

func personMsg(id int64, u telegram.User, text string) telegram.Update {
	return telegram.Update{UpdateID: id, Message: &telegram.Message{From: &u, Chat: telegram.Chat{ID: u.ID, Type: "private"}, Text: text}}
}

func personPress(id int64, u telegram.User, data string) telegram.Update {
	return telegram.Update{UpdateID: id, Callback: &telegram.Callback{ID: "cb", From: &u, Data: data,
		Message: &telegram.Message{MessageID: 9, Chat: telegram.Chat{ID: u.ID, Type: "private"}}}}
}

// to is what the bots sent to one chat.
func (f *fakeTG) to(chat int64) []sentMsg {
	var out []sentMsg
	for _, m := range f.messages() {
		if m.Chat == chat {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeTG) photoCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.photos)
}

// bots sets up a data directory with the owner bot and the client bot.
func bots(t *testing.T) (Store, *fakeTG, *bot, *clientBot) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	s := Store{Root: home}
	s.saveBotConfig(botConfig{Token: testToken, Chat: 42})
	writeFile(t, s.poolPath(), "mailru "+docB+"\n")
	f := newFakeTG(t)
	c, _ := s.loadBotConfig()
	owner := newBot(s, c)
	return s, f, owner, newClientBot(s, clientBotConfig{Token: testToken}, owner)
}

var dima = telegram.User{ID: 101, Username: "dima", FirstName: "Dima", LanguageCode: "ru"}

func TestClientBotFromRequestToLink(t *testing.T) {
	s, f, owner, cb := bots(t)
	cb.handle(personMsg(1, dima, "/start"))
	if m := f.to(101); len(m) != 1 || !strings.Contains(m[0].Text, "попросить доступ") || !strings.Contains(m[0].Buttons.String(), `"ask"`) {
		t.Fatalf("intro %+v", m)
	}
	cb.handle(personPress(2, dima, "ask"))
	if !strings.Contains(f.lastEditText(), "кто вы") {
		t.Errorf("ask:\n%s", f.lastEditText())
	}
	cb.handle(personMsg(3, dima, "брат   Олег"))
	r, err := s.getRequest(101)
	if err != nil || r.State != reqPending || r.Text != "брат Олег" || r.Lang != "ru" {
		t.Fatalf("request %+v %v", r, err)
	}
	if m := f.to(42); len(m) != 1 || !strings.Contains(m[0].Text, "Dima (@dima)</b> просит доступ") {
		t.Errorf("the owner was not told at once: %+v", m)
	}
	if m := f.to(101); !strings.Contains(m[len(m)-1].Text, "Заявка отправлена") {
		t.Errorf("person: %q", m[len(m)-1].Text)
	}
	// Group chats and other people get nothing of it.
	n := len(f.messages())
	cb.handle(telegram.Update{UpdateID: 4, Message: &telegram.Message{From: &dima, Chat: telegram.Chat{ID: -5, Type: "group"}, Text: "/start"}})
	if len(f.messages()) != n {
		t.Error("answered a group")
	}
	// The owner approves; the client bot delivers.
	owner.handle(press(5, 42, "rq:101:+30", time.Now()))
	photos := f.photoCount()
	cb.deliver(time.Now())
	m := f.to(101)
	if len(m) < 2 || !strings.Contains(m[len(m)-2].Text, "Доступ выдан") || f.photoCount() != photos+1 {
		t.Fatalf("delivery: %+v", m)
	}
	key, _ := s.Key("dima")
	if !strings.Contains(m[len(m)-1].Text, key) || !strings.Contains(m[len(m)-1].Text, "openflux://") {
		t.Errorf("access text %q", m[len(m)-1].Text)
	}
	if _, err := s.getRequest(101); err == nil {
		t.Error("the delivered request stays")
	}
	n = len(f.messages())
	cb.deliver(time.Now())
	if len(f.messages()) != n {
		t.Error("delivered twice")
	}
	// The channel screen; the link again, not too often.
	cb.handle(personMsg(6, dima, "/start"))
	if m := f.to(101); !strings.Contains(m[len(m)-1].Text, "Ваш канал: <b>dima</b>") {
		t.Errorf("channel: %q", m[len(m)-1].Text)
	}
	cb.handle(personPress(7, dima, "qr"))
	if !strings.Contains(f.lastEditText(), "раз в 2 минуты") {
		t.Errorf("rate limit:\n%s", f.lastEditText())
	}
	cb.qrAt[101] = time.Now().Add(-time.Hour)
	cb.handle(personPress(8, dima, "qr"))
	if f.photoCount() != photos+2 {
		t.Error("the link was not sent again")
	}
	// Somebody else gets only the intro.
	cb.handle(personMsg(9, telegram.User{ID: 202, Username: "x"}, "/qr"))
	if m := f.to(202); len(m) != 1 || strings.Contains(m[0].Text, key) || !strings.Contains(m[0].Text, "попросить доступ") {
		t.Errorf("a stranger's /qr: %+v", m)
	}
}

func TestClientBotRemindsAndExtends(t *testing.T) {
	s, f, owner, cb := bots(t)
	c, _ := s.Add("dima", "mailru", testURL)
	c.Telegram = &TGAccount{ID: 101, Username: "dima"}
	c.Expires = time.Now().Add(50 * time.Hour)
	s.Save(c)
	cb.deliver(time.Now())
	m := f.to(101)
	if len(m) != 1 || !strings.Contains(m[0].Text, "заканчивается") || !strings.Contains(m[0].Buttons.String(), `"ext"`) {
		t.Fatalf("reminder %+v", m)
	}
	cb.deliver(time.Now())
	if len(f.to(101)) != 1 {
		t.Error("reminded twice")
	}
	if st := newClientBot(s, clientBotConfig{Token: testToken}, nil); !st.st.Reminded["dima"].Equal(c.Expires) {
		t.Error("the reminder is not remembered across restarts")
	}
	cb.handle(personPress(1, dima, "ext"))
	if r, err := s.getRequest(101); err != nil || r.Kind != reqExtend || r.Client != "dima" {
		t.Fatalf("extension request %+v %v", r, err)
	}
	if m := f.to(42); len(m) != 1 || !strings.Contains(m[0].Text, "просит продлить доступ к <b>dima</b>") {
		t.Errorf("owner: %+v", m)
	}
	owner.handle(press(2, 42, "rq:101:+30", time.Now()))
	cb.deliver(time.Now())
	if m := f.to(101); !strings.Contains(m[len(m)-1].Text, "продлён") {
		t.Errorf("extended: %q", m[len(m)-1].Text)
	}
	if c2, _ := s.Get("dima"); !c2.Expires.Equal(c.Expires.AddDate(0, 0, 30)) {
		t.Errorf("expires %v", c2.Expires)
	}
	// An end in the last days is announced once; an old one is not.
	c2, _ := s.Get("dima")
	c2.Expires = time.Now().Add(-time.Hour)
	s.Save(c2)
	cb.deliver(time.Now())
	cb.deliver(time.Now())
	ended := 0
	for _, m := range f.to(101) {
		if strings.Contains(m.Text, "закончился") {
			ended++
		}
	}
	if ended != 1 {
		t.Errorf("ended told %d times", ended)
	}
	c2.Expires = time.Now().AddDate(0, -1, 0)
	s.Save(c2)
	n := len(f.messages())
	cb.deliver(time.Now())
	if len(f.messages()) != n {
		t.Error("an old end announced")
	}
}

func TestClientBotRejectsAndBlocks(t *testing.T) {
	s, f, owner, cb := bots(t)
	cb.handle(personPress(1, dima, "ask!"))
	cb.handle(personPress(2, dima, "wd")) // withdrawn
	if _, err := s.getRequest(101); err == nil {
		t.Fatal("not withdrawn")
	}
	cb.handle(personPress(3, dima, "ask!"))
	owner.handle(press(4, 42, "rq:101:rej", time.Now()))
	cb.deliver(time.Now())
	cb.deliver(time.Now())
	told := 0
	for _, m := range f.to(101) {
		if strings.Contains(m.Text, "отклонил") {
			told++
		}
	}
	if told != 1 {
		t.Errorf("rejection told %d times", told)
	}
	cb.handle(personMsg(5, dima, "/start"))
	if m := f.to(101); !strings.Contains(m[len(m)-1].Text, "Попросить снова можно с") {
		t.Errorf("after the rejection: %q", m[len(m)-1].Text)
	}
	owner.handle(press(6, 42, "rq:101:blk", time.Now()))
	s.decideRequest(101, reqBlocked, time.Now())
	cb.handle(personMsg(7, dima, "/start"))
	if m := f.to(101); m[len(m)-1].Text != "Доступ закрыт." {
		t.Errorf("blocked: %q", m[len(m)-1].Text)
	}
}

func TestClientBotLanguageAndHelp(t *testing.T) {
	s, f, _, cb := bots(t)
	writeFile(t, s.clientHelpPath(), "App: <https://example.org/app.apk>\n")
	en := telegram.User{ID: 303, Username: "ann", LanguageCode: "en-GB"}
	cb.handle(personMsg(1, en, "/help"))
	m := f.to(303)
	if len(m) != 1 || !strings.Contains(m[0].Text, "How to connect") || !strings.Contains(m[0].Text, "&lt;https://example.org/app.apk&gt;") {
		t.Errorf("help %+v", m)
	}
}

func TestClientBotSetup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	newFakeTG(t)
	s := Store{Root: home}
	var out strings.Builder
	if err := run([]string{"bot", "client"}, strings.NewReader(testToken+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	if c, ok, _ := s.loadClientBot(); !ok || c.Token != testToken || !strings.Contains(out.String(), "https://t.me/reflux_test_bot") {
		t.Errorf("setup: %+v\n%s", c, out.String())
	}
	if fi, _ := os.Stat(s.clientBotPath()); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	s.saveBotConfig(botConfig{Token: testToken, Chat: 42})
	if err := run([]string{"bot", "client"}, strings.NewReader(testToken+"\n"), io.Discard); err == nil {
		t.Error("the owner bot's token taken for the client bot")
	}
	if err := run([]string{"bot", "client", "off"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.loadClientBot(); ok {
		t.Error("still on")
	}
}
