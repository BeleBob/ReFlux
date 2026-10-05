package main

import (
	"encoding/json"
	"html"
	"log"
	"os"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/telegram"
)

// Messages between the owner and the clients: a person's "it does not
// work" from the client bot, with what the server knows of their channel,
// and the owner's message to every client.

const sosEvery = 10 * time.Minute

// sosAsk asks the person what does not work. The caller holds cb.mu.
func (cb *clientBot) sosAsk(u *telegram.User) screen {
	l := langOf(u.LanguageCode)
	if _, ok := cb.s.clientOf(u.ID); !ok {
		return cb.home(u)
	}
	if at, ok := cb.sosAt[u.ID]; ok && time.Since(at) < sosEvery {
		return screen{tr(l, "cb.sos.wait", int(sosEvery.Minutes())), keyboard{{cb.btn(l, "b.refresh", "home")}}}
	}
	cb.reporting[u.ID] = time.Now()
	return screen{tr(l, "cb.sos.ask"), keyboard{{cb.btn(l, "b.cb.sos.send", "sos!")}, {cb.btn(l, "b.cancel", "home")}}}
}

// sos tells the owner a person's channel does not work for them, with
// what the server sees of it. The caller holds cb.mu.
func (cb *clientBot) sos(u *telegram.User, text string) screen {
	l := langOf(u.LanguageCode)
	delete(cb.reporting, u.ID)
	c, ok := cb.s.clientOf(u.ID)
	if !ok {
		return cb.home(u)
	}
	if at, ok := cb.sosAt[u.ID]; ok && time.Since(at) < sosEvery {
		return screen{tr(l, "cb.sos.wait", int(sosEvery.Minutes())), keyboard{{cb.btn(l, "b.refresh", "home")}}}
	}
	cb.sosAt[u.ID] = time.Now()
	cb.s.logEvents([]event{{At: time.Now(), Level: "warn",
		RU: tr(langRU, "ev.sos", c.Name), EN: tr(langEN, "ev.sos", c.Name)}})
	if o := cb.owner; o != nil {
		report := cb.sosReport(o.lang, u, c, text)
		o.mu.Lock()
		o.t.SendKeyboard(o.chat, report, keyboard{{o.btn("b.client", "c:"+c.Name, c.Name), o.btn("b.logs", "lg:"+c.Name)}})
		o.mu.Unlock()
	}
	sc := cb.home(u)
	sc.text = tr(l, "cb.sos.sent") + "\n\n" + sc.text
	return sc
}

// sosReport is what the owner gets: who, what they wrote, and the
// channel as the server sees it.
func (cb *clientBot) sosReport(l lang, u *telegram.User, c Client, text string) string {
	e := html.EscapeString
	now := time.Now()
	who := accessRequest{ID: u.ID, Username: u.Username, Name: u.FirstName}.Who()
	var t strings.Builder
	t.WriteString(tr(l, "ui.sos", e(who), e(c.Name)))
	if text = strings.Join(strings.Fields(text), " "); text != "" {
		if len([]rune(text)) > 500 {
			text = string([]rune(text)[:500]) + "…"
		}
		t.WriteString("\n«" + e(text) + "»")
	}
	v := viewClients(cb.s, []Client{c})[0]
	p := accessPhrase(c, now)
	t.WriteString("\n\n" + tr(l, "ui.client.access", tr(l, p.id, p.args...)))
	t.WriteString("\n" + tr(l, "ui.sos.node", stateText(l, v)))
	if c.session() && v.status != nil && v.status.doc >= 0 {
		t.WriteString("\n" + tr(l, "ui.sos.doc", v.status.doc+1, len(c.Docs())))
	}
	if st, err := readEgressStatus(); err == nil {
		world, russia := "✅", "✅"
		if !st.WorldOK {
			world = "❌"
		}
		if !st.RUOK {
			russia = "❌"
		} else if st.RUFallback {
			russia = "⚠️"
		}
		t.WriteString("\n" + tr(l, "ui.sos.tunnels", world, russia))
	} else {
		t.WriteString("\n" + tr(l, "ui.egress.none"))
	}
	return t.String()
}

// broadcast sends the owner's message to every active client linked to
// Telegram, in their language, and says how many it reached. It does not
// take cb.mu: the owner bot calls it holding its own lock, and the client
// bot takes the two the other way round.
func (cb *clientBot) broadcast(text string) (sent, failed int) {
	var st clientBotState
	if b, err := os.ReadFile(cb.s.clientBotStatePath()); err == nil {
		json.Unmarshal(b, &st)
	}
	clients, _ := cb.s.List()
	now := time.Now()
	for _, c := range clients {
		if c.Telegram == nil || !c.Active(now) {
			continue
		}
		l := langOf(st.Lang[c.Telegram.ID])
		if _, err := cb.t.Send(c.Telegram.ID, tr(l, "cb.broadcast", html.EscapeString(text))); err != nil {
			log.Printf("client bot: message to %s: %v", c.Name, err)
			failed++
			continue
		}
		sent++
	}
	return sent, failed
}

// broadcastTargets counts the clients a message would reach.
func (s Store) broadcastTargets() int {
	clients, _ := s.List()
	n := 0
	for _, c := range clients {
		if c.Telegram != nil && c.Active(time.Now()) {
			n++
		}
	}
	return n
}

// ---- the owner bot ----

// broadcastAsk asks the owner for the message.
func (b *bot) broadcastAsk() screen {
	if b.clientBot == nil {
		return screen{b.tr("ui.bc.nobot"), keyboard{{b.btn("b.clients", "cls")}}}
	}
	b.await = awaiting{kind: "broadcast", at: time.Now()}
	return screen{b.tr("ui.bc.ask", b.s.broadcastTargets()), keyboard{{b.btn("b.cancel", "cls")}}}
}

// broadcastConfirm shows the message before it goes.
func (b *bot) broadcastConfirm(text string) screen {
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > 2000 {
		return b.broadcastAsk()
	}
	b.broadcastText = text
	return screen{b.tr("ui.bc.confirm", b.s.broadcastTargets(), html.EscapeString(text)),
		keyboard{{b.btn("b.bc.send", "bc!:"+stamp())}, {b.btn("b.cancel", "cls")}}}
}

func (b *bot) broadcastSend(at string) screen {
	if !fresh(at) || b.broadcastText == "" || b.clientBot == nil {
		return b.clientsScreen()
	}
	sent, failed := b.clientBot.broadcast(b.broadcastText)
	b.broadcastText = ""
	sc := b.clientsScreen()
	sc.text = b.tr("ui.bc.done", sent, failed) + "\n\n" + sc.text
	return sc
}
