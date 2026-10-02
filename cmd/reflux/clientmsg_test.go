package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSOSFromTheClientBot(t *testing.T) {
	s, f, _, cb := bots(t)
	c, _ := s.Add("dima", "mailru", testURL)
	c.Telegram = &TGAccount{ID: 101, Username: "dima"}
	s.Save(c)
	cb.handle(personMsg(1, dima, "/start"))
	if m := f.to(101); !strings.Contains(m[len(m)-1].Buttons.String(), `"sos"`) {
		t.Fatalf("no SOS button: %+v", m)
	}
	cb.handle(personPress(2, dima, "sos"))
	if !strings.Contains(f.lastEditText(), "Что не работает") {
		t.Errorf("ask:\n%s", f.lastEditText())
	}
	cb.handle(personMsg(3, dima, "не грузит   с утра"))
	o := f.to(42)
	if len(o) != 1 || !strings.Contains(o[0].Text, "<b>Dima (@dima)</b> (клиент <b>dima</b>) пишет, что канал не работает") ||
		!strings.Contains(o[0].Text, "«не грузит с утра»") || !strings.Contains(o[0].Text, "Сейчас:") || !strings.Contains(o[0].Buttons.String(), "c:dima") {
		t.Fatalf("owner: %+v", o)
	}
	if m := f.to(101); !strings.Contains(m[len(m)-1].Text, "отправлено владельцу") {
		t.Errorf("person: %q", m[len(m)-1].Text)
	}
	if evs := s.readEvents(1); len(evs) != 1 || !strings.Contains(evs[0].RU, "пишет, что канал не работает") {
		t.Errorf("event %+v", evs)
	}
	// Not more than once in ten minutes.
	cb.handle(personPress(4, dima, "sos"))
	if !strings.Contains(f.lastEditText(), "раз в 10 минут") {
		t.Errorf("limit:\n%s", f.lastEditText())
	}
	cb.handle(personPress(5, dima, "sos!"))
	if len(f.to(42)) != 1 {
		t.Error("a second report within ten minutes")
	}
	// Someone without a channel gets the intro.
	cb.handle(personPress(6, tgUser{ID: 202}, "sos"))
	if !strings.Contains(f.lastEditText(), "попросить доступ") {
		t.Errorf("no channel:\n%s", f.lastEditText())
	}
}

func TestBroadcastToTheClients(t *testing.T) {
	s, f, owner, cb := bots(t)
	add := func(name, url string, tg int64, paused bool) {
		c, _ := s.Add(name, "mailru", url)
		if tg != 0 {
			c.Telegram = &TGAccount{ID: tg}
		}
		c.Paused = paused
		s.Save(c)
	}
	add("dima", testURL, 101, false)
	add("olga", docB, 102, true) // paused: left out
	add("anon", docC, 0, false)  // not linked: left out
	// Without the client bot there is no button.
	if sc := owner.clientsScreen(); strings.Contains(fmt.Sprint(sc.kb), `"bc"`) {
		t.Error("broadcast offered without the client bot")
	}
	owner.clientBot = cb
	owner.handle(press(1, 42, "cls", time.Now()))
	if !strings.Contains(fmt.Sprint(owner.clientsScreen().kb), "bc") {
		t.Fatal("no broadcast button")
	}
	owner.handle(press(2, 42, "bc", time.Now()))
	if !strings.Contains(f.lastEditText(), "сейчас их 1") {
		t.Errorf("ask:\n%s", f.lastEditText())
	}
	owner.handle(msg(3, 42, "Сервер перезагрузится\nв 23:00"))
	m := f.to(42)
	if !strings.Contains(m[len(m)-1].Text, "Отправить это клиентам (1)?") || !strings.Contains(m[len(m)-1].Text, "Сервер перезагрузится\nв 23:00") {
		t.Fatalf("confirm %q", m[len(m)-1].Text)
	}
	owner.handle(press(4, 42, "bc!:"+fmt.Sprint(time.Now().Add(-time.Hour).Unix()), time.Now()))
	if len(f.to(101)) != 0 {
		t.Error("a stale button sent")
	}
	owner.handle(press(5, 42, "bc", time.Now()))
	owner.handle(msg(6, 42, "Сервер перезагрузится"))
	owner.handle(press(7, 42, "bc!:"+stamp(), time.Now()))
	if p := f.to(101); len(p) != 1 || !strings.Contains(p[0].Text, "Сообщение от владельца") {
		t.Errorf("client: %+v", p)
	}
	if len(f.to(102)) != 0 {
		t.Error("a paused client got it")
	}
	if !strings.Contains(f.lastEditText(), "Отправлено: 1, не доставлено: 0") {
		t.Errorf("result:\n%s", f.lastEditText())
	}
}
