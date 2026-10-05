package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/i18n"
	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/telegram"
)

const testToken = "123456:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"

// fakeTG is a Bot API server. Updates in arrive are released one per
// getUpdates call after the first, as if sent while the bot waits.
type fakeTG struct {
	mu      sync.Mutex
	pending []telegram.Update
	arrive  []telegram.Update
	calls   int
	sent    []sentMsg
	failing bool     // sendMessage fails
	photos  []string // captions
	deleted []int64
	edited  []string
	methods []string
}

type sentMsg struct {
	Chat    int64   `json:"chat_id"`
	Text    string  `json:"text"`
	Buttons rawJSON `json:"reply_markup"`
}

type rawJSON json.RawMessage

func (r *rawJSON) UnmarshalJSON(b []byte) error { *r = append((*r)[:0], b...); return nil }
func (r rawJSON) String() string                { return string(r) }

func newFakeTG(t *testing.T) *fakeTG {
	f := &fakeTG{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, ok := strings.CutPrefix(r.URL.Path, "/bot"+testToken+"/")
		if !ok {
			w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		reply := func(v any) {
			b, _ := json.Marshal(map[string]any{"ok": true, "result": v})
			w.Write(b)
		}
		f.methods = append(f.methods, method)
		switch method {
		case "sendPhoto":
			r.ParseMultipartForm(1 << 20)
			if _, _, err := r.FormFile("photo"); err != nil {
				t.Errorf("sendPhoto without a photo: %v", err)
			}
			f.photos = append(f.photos, r.FormValue("caption"))
			reply(map[string]any{"message_id": 1000 + len(f.photos)})
		case "deleteMessage":
			var p struct {
				ID int64 `json:"message_id"`
			}
			json.NewDecoder(r.Body).Decode(&p)
			f.deleted = append(f.deleted, p.ID)
			reply(true)
		case "editMessageText":
			var p struct{ Text string }
			json.NewDecoder(r.Body).Decode(&p)
			f.edited = append(f.edited, p.Text)
			reply(map[string]any{"message_id": 1})
		case "answerCallbackQuery", "setMyCommands":
			reply(true)
		case "getMe":
			reply(telegram.User{ID: 1, FirstName: "ReFlux", Username: "reflux_test_bot"})
		case "getUpdates":
			var p struct{ Offset int64 }
			json.NewDecoder(r.Body).Decode(&p)
			if f.calls++; f.calls > 1 && len(f.arrive) > 0 {
				f.pending = append(f.pending, f.arrive[0])
				f.arrive = f.arrive[1:]
			}
			var out []telegram.Update
			for _, u := range f.pending {
				if u.UpdateID >= p.Offset || p.Offset < 0 {
					out = append(out, u)
				}
			}
			if p.Offset < 0 && len(out) > 0 {
				out = out[len(out)-1:]
			}
			reply(out)
		case "sendMessage":
			if f.failing {
				w.Write([]byte(`{"ok":false,"error_code":502,"description":"Bad Gateway"}`))
				return
			}
			var m sentMsg
			json.NewDecoder(r.Body).Decode(&m)
			f.sent = append(f.sent, m)
			reply(map[string]any{"message_id": len(f.sent)})
		default:
			w.Write([]byte(`{"ok":false,"error_code":404,"description":"Not Found"}`))
		}
	}))
	old := telegram.API
	telegram.API = srv.URL
	t.Cleanup(func() { telegram.API = old; srv.Close() })
	return f
}

func (f *fakeTG) messages() []sentMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMsg(nil), f.sent...)
}

func msg(id, chat int64, text string) telegram.Update {
	return telegram.Update{UpdateID: id, Message: &telegram.Message{
		From: &telegram.User{ID: chat, FirstName: "Owner", Username: "owner"},
		Chat: telegram.Chat{ID: chat, Type: "private"}, Text: text,
	}}
}

func TestBotSetupLinksTheOwnersChat(t *testing.T) {
	s := Store{Root: t.TempDir()}
	f := newFakeTG(t)
	f.pending = []telegram.Update{msg(5, 666, "/start")} // sent before setup: not the owner's answer
	f.arrive = []telegram.Update{msg(6, 42, "/start")}
	var out strings.Builder
	if err := botSetup(s, strings.NewReader(testToken+"\nyes\n"), &out, 0); err == nil {
		t.Fatal("setup with no time to wait succeeded")
	}
	f.calls = 0
	if err := botSetup(s, strings.NewReader(testToken+"\nyes\n"), &out, 5e9); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	c, err := s.loadBotConfig()
	if err != nil || c.Chat != 42 || c.Token != testToken {
		t.Fatalf("config = %+v, %v", c, err)
	}
	if st, _ := os.Stat(s.botConfigPath()); st.Mode().Perm() != 0o600 {
		t.Errorf("telegram.json mode %v, want 0600", st.Mode().Perm())
	}
	if m := f.messages(); len(m) != 1 || m[0].Chat != 42 {
		t.Errorf("sent %+v, want one message to the owner", m)
	}
	if !strings.Contains(out.String(), "https://t.me/reflux_test_bot") {
		t.Errorf("no link to the bot in:\n%s", out.String())
	}
}

func TestBotSetupRefusesWhatIsNotAToken(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if err := botSetup(s, strings.NewReader("hello\n"), io.Discard, 0); err == nil {
		t.Error("accepted a non-token")
	}
	if _, err := os.Stat(s.botConfigPath()); err == nil {
		t.Error("config written")
	}
}

// The token is in every request URL, and the HTTP client quotes the URL in
// its errors; the bot logs errors to the journal.
func TestTelegramErrorsHideTheToken(t *testing.T) {
	old := telegram.API
	telegram.API = "http://127.0.0.1:1"
	defer func() { telegram.API = old }()
	_, err := telegram.New(testToken).GetMe()
	if err == nil {
		t.Fatal("no error from a closed port")
	}
	if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "AAHdqTcv") {
		t.Errorf("the error shows the token: %v", err)
	}
}

func TestBotAnswersOnlyItsOwner(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	if err := run([]string{"add", "guest", "--url", testURL, "--no-apply"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	s := Store{Root: home}
	f := newFakeTG(t)
	b := newBot(s, botConfig{Token: testToken, Chat: 42, Lang: "en"})

	b.handle(msg(1, 7, "/pause guest"))
	if c, _ := s.Get("guest"); c.Paused || len(f.messages()) != 0 {
		t.Fatal("a stranger's command was obeyed or answered")
	}
	b.handle(msg(2, 42, "/help"))
	b.handle(msg(3, 42, "/pause guest"))
	if c, _ := s.Get("guest"); !c.Paused {
		t.Error("/pause from the owner did not pause")
	}
	b.handle(msg(4, 42, "/status"))
	m := f.messages()
	if len(m) != 3 || !strings.Contains(m[0].Text, "/doctor") || !strings.Contains(m[1].Text, "Access: paused") ||
		!strings.Contains(m[2].Text, "<b>guest</b> · paused") {
		t.Errorf("replies = %+v", m)
	}
}

func init() { i18n.Messages["test.raw"] = [2]string{"%s", "%s"} }

func TestMonitorReportsLastingChangesOnly(t *testing.T) {
	m := monitor{confirm: 2}
	world := func(lv level, server string) finding {
		state := map[level]string{levelOK: "up", levelFail: "DOWN"}[lv]
		return finding{Key: "world", Level: lv, Msg: "test.raw", Args: []any{"world " + state + " via " + server}, Sig: state + " " + server}
	}
	node := func(lv level) finding {
		return finding{Key: "node:phone", Level: lv, Msg: "test.raw", Args: []any{"node phone " + lv.String()}, Sig: lv.String()}
	}
	steps := []struct {
		fs   []finding
		news string
	}{
		{[]finding{world(levelOK, "world-3.conf"), node(levelOK)}, ""}, // baseline
		{[]finding{world(levelFail, "world-3.conf"), node(levelOK)}, ""},
		{[]finding{world(levelFail, "world-3.conf"), node(levelOK)}, "❌ world DOWN via world-3.conf"},
		{[]finding{world(levelOK, "world-4.conf"), node(levelFail)}, ""},
		{[]finding{world(levelOK, "world-4.conf"), node(levelOK)}, "✅ world up via world-4.conf"}, // the node blinked
		{[]finding{world(levelOK, "world-5.conf"), node(levelOK)}, ""},
		{[]finding{world(levelOK, "world-5.conf"), node(levelOK)}, "ℹ️ world up via world-5.conf"}, // failover
		{[]finding{world(levelOK, "world-5.conf")}, ""},
		{[]finding{world(levelOK, "world-5.conf")}, ""}, // the client was revoked: no news
		{[]finding{world(levelOK, "world-5.conf"), node(levelOK)}, ""},
	}
	for i, st := range steps {
		var texts []string
		for _, a := range m.update(st.fs, langEN) {
			texts = append(texts, a.text(langEN))
		}
		if got := strings.Join(texts, "\n"); got != st.news {
			t.Errorf("run %d: news %q, want %q", i, got, st.news)
		}
	}
}

func TestBotKeepsAlertsItCouldNotSend(t *testing.T) {
	f := newFakeTG(t)
	b := newBot(Store{Root: t.TempDir()}, botConfig{Token: testToken, Chat: 42, Lang: "en"})
	f.failing = true
	b.deliver([]string{"first"})
	f.mu.Lock()
	f.failing = false
	f.mu.Unlock()
	b.deliver([]string{"second"})
	m := f.messages()
	if len(m) != 2 || m[0].Text != "first" || m[1].Text != "second" {
		t.Errorf("sent %+v, want first then second", m)
	}
}

func TestBotInstallWritesAUserService(t *testing.T) {
	home := t.TempDir()
	s := Store{Root: filepath.Join(home, "reflux")}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	if err := botInstall(s, io.Discard); err == nil {
		t.Error("installed without a linked bot")
	}
	if err := s.saveBotConfig(botConfig{Token: testToken, Chat: 42, Lang: "en"}); err != nil {
		t.Fatal(err)
	}
	var calls []string
	old := runCmd
	runCmd = func(stdout io.Writer, name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if name == "loginctl" {
			io.WriteString(stdout, "Linger=no\n")
		}
		return nil
	}
	defer func() { runCmd = old }()
	var out strings.Builder
	if err := botInstall(s, &out); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(filepath.Join(home, "config", "systemd", "user", "reflux-bot.service"))
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	for _, want := range []string{"ExecStart=" + exe + " bot run\n", "Environment=REFLUX_HOME=" + s.Root + "\n", "Restart=always\n"} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(string(unit), testToken) {
		t.Error("the unit holds the token")
	}
	if got := strings.Join(calls, "\n"); !strings.Contains(got, "systemctl --user restart reflux-bot.service") {
		t.Errorf("calls:\n%s", got)
	}
	if !strings.Contains(out.String(), "enable-linger") {
		t.Errorf("no linger hint:\n%s", out.String())
	}
}

func TestBotHomeScreen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	if err := run([]string{"add", "phone", "--url", testURL, "--no-apply"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	runDocker = func(stdout io.Writer, args ...string) error {
		switch strings.Join(args, " ") {
		case "exec reflux-egress cat /run/reflux-egress/status.json":
			io.WriteString(stdout, `{"world":"world-3.conf","world_ok":true,"ru_ok":true,"ru_mode":"direct","killswitch_dropped":0}`)
		case "ps --all --filter name=^reflux- --format {{.Names}}\t{{.Status}}":
			io.WriteString(stdout, "reflux-node-phone\tUp 2 hours\n")
		}
		return nil
	}
	for l, want := range map[string][]string{
		"en": {"🌍 World: ✅ world-3.conf", "🇷🇺 Russia: ✅ direct", "🟡 <b>phone</b> · no status", "🔄 Refresh"},
		"ru": {"🌍 Мир: ✅ world-3.conf", "🇷🇺 Россия: ✅ напрямую", "🟡 <b>phone</b> · нет статуса", "🔄 Обновить"},
	} {
		sc := newBot(Store{Root: home}, botConfig{Chat: 42, Lang: l}).home()
		all := sc.text
		for _, row := range sc.kb {
			for _, b := range row {
				all += "\n" + b.Text
			}
		}
		for _, w := range want {
			if !strings.Contains(all, w) {
				t.Errorf("%s: home lacks %q:\n%s", l, w, all)
			}
		}
	}
}

func press(id, chat int64, data string, sent time.Time) telegram.Update {
	return telegram.Update{UpdateID: id, Callback: &telegram.Callback{
		ID: "cb", From: &telegram.User{ID: chat}, Data: data,
		Message: &telegram.Message{MessageID: 77, Date: sent.Unix(), Chat: telegram.Chat{ID: chat, Type: "private"}},
	}}
}

func TestBotManagesClients(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	calls := fakeDocker(t, "")
	s := Store{Root: home}
	f := newFakeTG(t)
	b := newBot(s, botConfig{Token: testToken, Chat: 42, Lang: "en"})
	old := showKeep
	showKeep = 100 * time.Millisecond
	defer func() { showKeep = old }()

	b.handle(msg(1, 42, "/add guest "+testURL+" 30d"))
	c, err := s.Get("guest")
	if err != nil || c.Expires.IsZero() {
		t.Fatalf("/add: client %+v, %v", c, err)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "up --detach --remove-orphans") {
		t.Errorf("/add did not start the node: %q", *calls)
	}

	b.handle(msg(2, 42, "/show guest"))
	key, _ := s.Key("guest")
	m := f.messages()
	if len(f.photos) != 1 || !strings.Contains(m[len(m)-1].Text, key) || !strings.Contains(m[len(m)-1].Text, "openflux://") {
		t.Fatalf("/show sent photos %q and %+v", f.photos, m[len(m)-1])
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.deleted)
		f.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the access data was not deleted: %v", f.deleted)
		}
		time.Sleep(20 * time.Millisecond)
	}

	b.handle(msg(3, 42, "/revoke guest"))
	m = f.messages()
	confirm := regexp.MustCompile(`rv!:guest:[0-9]+`).FindString(m[len(m)-1].Buttons.String())
	if confirm == "" {
		t.Fatalf("/revoke did not ask for a confirmation: %+v", m[len(m)-1])
	}
	if _, err := s.Get("guest"); err != nil {
		t.Fatal("revoked before the confirmation")
	}
	stale := fmt.Sprintf("rv!:guest:%d", time.Now().Add(-time.Hour).Unix())
	b.handle(press(4, 7, confirm, time.Now())) // a stranger's press
	b.handle(press(5, 42, stale, time.Now()))  // an old button
	if _, err := s.Get("guest"); err != nil {
		t.Fatal("revoked by a stranger or by a stale button")
	}
	b.handle(press(6, 42, confirm, time.Now()))
	if _, err := s.Get("guest"); err == nil {
		t.Error("not revoked after the confirmation")
	}
	if len(f.edited) != 2 || !strings.Contains(f.edited[0], "Access:") || !strings.Contains(f.edited[1], "<b>guest</b> revoked") {
		t.Errorf("edits = %q", f.edited)
	}
}

func TestBotLogsKeepTheEnd(t *testing.T) {
	t.Setenv("REFLUX_HOME", t.TempDir())
	fakeDocker(t, "")
	runDocker = func(stdout io.Writer, args ...string) error {
		for i := 0; i < 400; i++ {
			fmt.Fprintf(dockerStderr, "line %03d of the egress log\n", i)
		}
		return nil
	}
	got := newBot(Store{Root: t.TempDir()}, botConfig{Chat: 42, Lang: "en"}).logs("egress")
	if !strings.Contains(got, "line 399") || strings.Contains(got, "line 000") || len(got) > 4096 {
		t.Errorf("logs reply (%d bytes) does not keep the end:\n%.200s", len(got), got)
	}
	if got := newBot(Store{Root: t.TempDir()}, botConfig{Chat: 42, Lang: "en"}).logs("../etc"); strings.Contains(got, "<pre>") {
		t.Errorf("a bad name reached docker: %s", got)
	}
}
