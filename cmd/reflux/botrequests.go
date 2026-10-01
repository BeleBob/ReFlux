package main

import (
	"fmt"
	"html"
	"io"
	"log"
	"strconv"
	"strings"
	"time"
)

// The owner bot's side of access requests (requests.go): a message with
// buttons for each new request, a screen of the waiting ones, and the
// decisions.

// requestText describes a request to the owner.
func (b *bot) requestText(r accessRequest) string {
	e := html.EscapeString
	var t strings.Builder
	if r.Kind == reqExtend {
		t.WriteString(b.tr("ui.rq.extend", e(r.Who()), e(r.Client)))
		if c, err := b.s.Get(r.Client); err == nil {
			p := accessPhrase(c, time.Now())
			t.WriteString("\n" + b.tr("ui.client.access", b.tr(p.id, p.args...)))
		}
	} else {
		t.WriteString(b.tr("ui.rq.access", e(r.Who())))
	}
	fmt.Fprintf(&t, "\n<code>%d</code> · %s", r.ID, r.At.Local().Format("02.01 15:04"))
	if r.Text != "" {
		t.WriteString("\n«" + e(r.Text) + "»")
	}
	if r.Kind == reqAccess {
		t.WriteString("\n\n" + b.tr("ui.docs.pool", b.s.freeCount()))
	}
	return t.String()
}

func (b *bot) requestKeyboard(r accessRequest) keyboard {
	id := strconv.FormatInt(r.ID, 10)
	return keyboard{
		{b.btn("b.rq.30", "rq:"+id+":+30"), b.btn("b.rq.90", "rq:"+id+":+90"), b.btn("b.rq.never", "rq:"+id+":never")},
		{b.btn("b.rq.reject", "rq:"+id+":rej"), b.btn("b.rq.block", "rq:"+id+":blk")},
	}
}

// notifyRequests tells the owner of requests they have not heard of. The
// caller holds b.mu.
func (b *bot) notifyRequests() {
	for _, r := range b.s.pendingRequests() {
		if r.Notified {
			continue
		}
		if _, err := b.t.sendKeyboard(b.chat, "📨 "+b.requestText(r), b.requestKeyboard(r)); err != nil {
			log.Printf("bot: request of %d: %v", r.ID, err)
			return // the next run tries again
		}
		r.Notified = true
		b.s.saveRequest(r)
		b.s.logEvents([]event{{At: time.Now(), Level: "info",
			RU: tr(langRU, "ev.rq", r.Who()), EN: tr(langEN, "ev.rq", r.Who())}})
	}
}

// requestsScreen lists the waiting requests.
func (b *bot) requestsScreen() screen {
	pending := b.s.pendingRequests()
	var t strings.Builder
	t.WriteString(b.tr("ui.rq.title", len(pending)) + "\n")
	var kb keyboard
	for _, r := range pending {
		kind := "📨"
		if r.Kind == reqExtend {
			kind = "⏳"
		}
		fmt.Fprintf(&t, "\n%s %s · %s", kind, html.EscapeString(r.Who()), r.At.Local().Format("02.01 15:04"))
		kb = append(kb, []tgButton{{Text: kind + " " + r.Who(), Data: "rq1:" + strconv.FormatInt(r.ID, 10)}})
	}
	if len(pending) == 0 {
		t.WriteString("\n" + b.tr("ui.rq.none"))
	}
	return screen{t.String(), append(kb, []tgButton{b.btn("b.home", "home")})}
}

// requestPress handles rq1:<id> (one request) and rq:<id>:<decision>.
func (b *bot) requestPress(action, arg string) screen {
	idText, how, _ := strings.Cut(arg, ":")
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil {
		return b.requestsScreen()
	}
	r, err := b.s.getRequest(id)
	if err != nil {
		return b.failed(err, b.btn("b.rq", "rqs", len(b.s.pendingRequests())))
	}
	if action == "rq1" || r.State != reqPending {
		if r.State != reqPending {
			return screen{b.requestText(r) + "\n\n" + b.tr("ui.rq.decided", b.tr("rq.state."+r.State)),
				keyboard{{b.btn("b.rq", "rqs", len(b.s.pendingRequests()))}}}
		}
		return screen{"📨 " + b.requestText(r), append(b.requestKeyboard(r), []tgButton{b.btn("b.back", "rqs")})}
	}
	now := time.Now()
	var note string
	var c Client
	err = b.change(func() error {
		switch how {
		case "rej", "blk":
			state := reqRejected
			if how == "blk" {
				state = reqBlocked
			}
			r, err = b.s.decideRequest(id, state, now)
			note = b.tr("ui.rq.decided", b.tr("rq.state."+state))
			return err
		}
		if r, c, err = b.s.approveRequest(id, how, now); err != nil {
			return err
		}
		p := accessPhrase(c, now)
		note = b.tr("ui.rq.approved", html.EscapeString(c.Name), b.tr(p.id, p.args...))
		return apply(b.s, io.Discard)
	})
	if err != nil {
		return b.failed(err, b.btn("b.back", "rq1:"+idText))
	}
	if r.State == reqApproved {
		b.s.logEvents([]event{{At: now, Level: "ok",
			RU: tr(langRU, "ev.rq.ok", r.Who(), c.Name), EN: tr(langEN, "ev.rq.ok", r.Who(), c.Name)}})
	}
	kb := keyboard{{b.btn("b.rq", "rqs", len(b.s.pendingRequests()))}}
	if c.Name != "" {
		kb = keyboard{{b.btn("b.client", "c:"+c.Name, c.Name), b.btn("b.rq", "rqs", len(b.s.pendingRequests()))}}
	}
	return screen{b.requestText(r) + "\n\n" + note, kb}
}
