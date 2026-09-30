package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openflux/transport/ipc"
)

func gatewayStore(t *testing.T) Store {
	t.Helper()
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	s := Store{Root: home}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"world-1.conf", "world-2.conf", "ru-1.conf"} {
		os.WriteFile(filepath.Join(s.egressDir(), f), []byte("x"), 0o600)
	}
	return s
}

func TestChooseTheWorldServer(t *testing.T) {
	s := gatewayStore(t)
	fakeDocker(t, "")
	if err := run([]string{"gateway", "world-2.conf"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if s.chosenWorld() != "world-2.conf" {
		t.Errorf("chosen %q", s.chosenWorld())
	}
	if st, _ := os.Stat(filepath.Join(s.egressDir(), worldSelect)); st.Mode().Perm() != 0o600 {
		t.Errorf("world-select mode %v", st.Mode().Perm())
	}
	if err := run([]string{"gateway", "world-7.conf"}, nil, io.Discard); err == nil || s.chosenWorld() != "world-2.conf" {
		t.Error("an unknown server was chosen")
	}
	if err := run([]string{"gateway", "auto"}, nil, io.Discard); err != nil || s.chosenWorld() != "" {
		t.Errorf("auto: %v, chosen %q", err, s.chosenWorld())
	}
}

func TestRussiaModeRestartsTheEgress(t *testing.T) {
	s := gatewayStore(t)
	calls := fakeDocker(t, "")
	for _, mode := range []string{"fallback", "direct", "tunnel"} {
		*calls = nil
		if err := run([]string{"gateway", "russia", mode}, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
		if got := s.russiaMode(); got != mode {
			t.Errorf("mode %q after setting %q", got, mode)
		}
		if !strings.Contains(strings.Join(*calls, "\n"), "--force-recreate") {
			t.Errorf("%s: egress not restarted: %q", mode, *calls)
		}
	}
	if err := run([]string{"gateway", "russia", "sideways"}, nil, io.Discard); err == nil {
		t.Error("an unknown mode accepted")
	}
	os.Remove(filepath.Join(s.egressDir(), "ru-1.conf"))
	if err := run([]string{"gateway", "russia", "fallback"}, nil, io.Discard); err == nil {
		t.Error("a tunnel mode accepted without ru-*.conf")
	}
}

func TestBotGateway(t *testing.T) {
	s := gatewayStore(t)
	fakeDocker(t, "")
	f := newFakeTG(t)
	b := newBot(s, botConfig{Token: testToken, Chat: 42, Lang: "ru"})
	b.handle(press(1, 42, "gw", time.Now()))
	b.handle(press(2, 42, "gws:world-2.conf", time.Now()))
	if s.chosenWorld() != "world-2.conf" {
		t.Errorf("chosen %q", s.chosenWorld())
	}
	if !strings.Contains(f.edited[1], "Ваш выбор: world-2") {
		t.Errorf("gateway screen:\n%s", f.edited[1])
	}
	b.handle(press(3, 42, "gwr:direct", time.Now()))
	if s.russiaMode() != "tunnel" {
		t.Fatal("the Russia mode changed before the confirmation")
	}
	b.handle(press(4, 42, "gwr!:direct:1", time.Now())) // a stale button
	if s.russiaMode() != "tunnel" {
		t.Fatal("a stale button changed the Russia mode")
	}
	b.handle(press(5, 42, "gwr!:direct:"+stamp(), time.Now()))
	if s.russiaMode() != "direct" {
		t.Errorf("mode %q after the confirmation", s.russiaMode())
	}
}

func TestNameAClientAfterItsTelegramNick(t *testing.T) {
	for in, want := range map[string]string{"Dima_Kost": "dima-kost", "_x_y_": "x-y", "": "", "a.b": ""} {
		if got := nick(TGAccount{Username: in}); got != want {
			t.Errorf("nick(%q) = %q, want %q", in, got, want)
		}
	}
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
	up.Callback.From = &tgUser{ID: 42, FirstName: "Дмитрий", Username: "Dima_Kost"}
	b.handle(up)
	if len(f.edited) != 1 || !strings.Contains(f.edited[0], "Telegram: Дмитрий (@Dima_Kost)") {
		t.Fatalf("after linking: %q", f.edited)
	}
	b.handle(press(2, 42, "rnk:phone", time.Now()))
	c, err := s.Get("dima-kost")
	if err != nil || c.Telegram == nil || c.Telegram.ID != 42 {
		t.Fatalf("renamed client %+v, %v", c, err)
	}
}

// The checks screen: a short line for what works, the full text with its
// hint for what does not, grouped.
func TestChecksScreenIsShort(t *testing.T) {
	s := fakeHost(t, hostState{
		module: true, worldUp: false, nodeStart: "2026-09-29T17:00:05Z", cron: "reflux heal", dfUsed: "86",
		nodeStatus: &ipc.StatusPayload{Running: true, Connected: true, UptimeMs: 5 * 3600e3, BytesOut: 1_400_000_000},
	})
	text := newBot(s, botConfig{Chat: 42, Lang: "ru"}).doctorScreen().text
	for _, want := range []string{
		"Проблем: 1", "🌐 <b>Туннели</b>", "👥 <b>Клиенты и ноды</b>", "📦 <b>Обновления</b>", "🖥 <b>Сервер</b>",
		"❌ мир: НЕ работает (world-1.conf): reflux logs egress",
		"✅ Россия: напрямую · 8652 сетей",
		"✅ phone: 5 ч · в сети · ↓1.4 GB ↑0 B",
		"✅ node: 9f4bef4 · ",
		"✅ Модуль ядра AWG", "✅ Диск /: занят на 86%",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("checks screen lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "ghcr.io") {
		t.Errorf("full image names on the checks screen:\n%s", text)
	}
}
