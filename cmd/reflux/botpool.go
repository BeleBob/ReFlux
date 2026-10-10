package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/telegram"
)

// The bot's side of the document checks (doccheck.go): the automatic check
// in its minute loop, the owner's news about it, the client's new link, and
// the documents screen with the dead list and its "check again" buttons.

// docsTick runs the automatic document check when it is due. It takes the
// store lock (skipping a busy minute) and b.mu only to tell the news.
func (b *bot) docsTick(now time.Time) {
	st, err := b.s.loadDocsState()
	if err != nil || now.Sub(st.Checked) < docCheckEvery {
		return
	}
	unlock, err := b.s.Lock(0)
	if err != nil {
		return
	}
	changes, changed, err := b.s.checkDocs(now)
	if changed {
		if err := apply(b.s, io.Discard); err != nil {
			log.Printf("bot: documents: apply: %v", err)
		}
	}
	if err != nil {
		log.Printf("bot: documents: %v", err)
		// Not every minute: the next try waits its turn too.
		if st, e := b.s.loadDocsState(); e == nil {
			st.Checked = now
			b.s.saveDocsState(st)
		}
	}
	unlock()
	if len(changes) == 0 {
		return
	}
	b.mu.Lock()
	var news []string
	var evs []event
	for _, ch := range changes {
		id, args := docChangeMessage(ch)
		evs = append(evs, event{At: now, Level: "warn", RU: tr(langRU, id, args...), EN: tr(langEN, id, args...)})
		if !b.mute["nodes"] {
			news = append(news, tr(b.lang, id, args...))
		}
	}
	if err := b.s.logEvents(evs); err != nil {
		log.Printf("bot: event log: %v", err)
	}
	cb := b.clientBot
	b.mu.Unlock()
	b.deliver(news)
	if cb == nil {
		return
	}
	for _, ch := range changes {
		if ch.New == nil || ch.Err != nil {
			continue
		}
		if c, err := b.s.Get(ch.Client); err == nil {
			cb.docReplaced(c)
		}
	}
}

// docChangeMessage tells the owner what happened to one dead document.
func docChangeMessage(ch docChange) (string, []any) {
	e := html.EscapeString
	reason := e(ch.Reason)
	switch {
	case ch.Client == "":
		return "docs.dead.pool", []any{e(ch.Doc.Transport), reason}
	case ch.Err != nil && ch.New == nil:
		return "docs.dead.nofree", []any{e(ch.Client), reason, e(ch.Err.Error())}
	case ch.Err != nil:
		return "docs.dead.failed", []any{e(ch.Client), reason, e(ch.Err.Error())}
	case ch.New == nil:
		return "docs.dead.dropped", []any{e(ch.Client), reason}
	}
	return "docs.dead.replaced", []any{e(ch.Client), reason}
}

// docKey names a document in a button without its link.
func docKey(url string) string {
	h := sha256.Sum256([]byte(url))
	return hex.EncodeToString(h[:6])
}

// poolScreen is the documents screen: the pool's counts, the last check
// and the dead documents with their buttons.
func (b *bot) poolScreen() screen {
	pool, err := b.s.poolStatus()
	if err != nil {
		return b.failed(err, b.btn("b.back", "home"))
	}
	st, err := b.s.loadDocsState()
	if err != nil {
		return b.failed(err, b.btn("b.back", "home"))
	}
	free, used, quarantined := 0, 0, 0
	for _, p := range pool {
		switch {
		case p.free():
			free++
		case p.State != nil && p.State.State == stateQuarantine:
			quarantined++
		case p.User != "":
			used++
		}
	}
	dead := st.deadDocs()
	var t strings.Builder
	t.WriteString(b.tr("ui.pool.title") + "\n")
	t.WriteString(b.tr("ui.pool.counts", free, used, quarantined, len(dead)) + "\n")
	if st.Checked.IsZero() {
		t.WriteString(b.tr("ui.pool.never", int(docCheckEvery.Minutes())) + "\n")
	} else {
		t.WriteString(b.tr("ui.pool.checked", st.Checked.Local().Format("02.01 15:04"), int(docCheckEvery.Minutes())) + "\n")
	}
	var kb keyboard
	if len(dead) > 0 {
		t.WriteString("\n" + b.tr("ui.pool.dead") + "\n")
		var row []telegram.Button
		for i, d := range dead {
			whose := b.tr("ui.pool.frompool")
			if d.Client != "" {
				whose = b.tr("ui.pool.from", html.EscapeString(d.Client))
			}
			fmt.Fprintf(&t, "%d. %s · %s · %s\n<code>%s</code>\n", i+1, html.EscapeString(d.Transport), whose,
				d.Since.Local().Format("02.01 15:04"), html.EscapeString(d.URL))
			if d.Reason != "" {
				t.WriteString("   " + html.EscapeString(d.Reason) + "\n")
			}
			if len(row) == 5 {
				kb = append(kb, row)
				row = nil
			}
			row = append(row, b.btn("b.pool.recheck", "pk:"+docKey(d.URL), i+1))
		}
		kb = append(kb, row)
		if len(dead) > 1 {
			kb = append(kb, []telegram.Button{b.btn("b.pool.recheckall", "pk:*")})
		}
	} else {
		t.WriteString("\n" + b.tr("ui.pool.nodead") + "\n")
	}
	kb = append(kb, []telegram.Button{b.btn("b.pool.checknow", "pchk")}, []telegram.Button{b.btn("b.refresh", "pl"), b.btn("b.back", "home")})
	return screen{t.String(), kb}
}

// poolPress runs the documents screen's checks, which take a few seconds:
// the caller answers the press first and holds neither b.mu nor the store
// lock.
func (b *bot) poolPress(action, arg string) screen {
	now := time.Now()
	unlock, err := b.s.Lock(lockWait)
	if err != nil {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.failed(err, b.btn("b.back", "pl"))
	}
	var note string
	switch action {
	case "pk":
		var urls []string
		if arg != "*" {
			st, _ := b.s.loadDocsState()
			for _, d := range st.deadDocs() {
				if docKey(d.URL) == arg {
					urls = append(urls, d.URL)
				}
			}
			if len(urls) == 0 {
				unlock()
				b.mu.Lock()
				defer b.mu.Unlock()
				sc := b.poolScreen()
				sc.text = b.tr("ui.expired.button") + "\n\n" + sc.text
				return sc
			}
		}
		back, still, err := b.s.recheckDead(urls, now)
		if err != nil {
			note = b.tr("ui.pool.checkfail", html.EscapeString(err.Error()))
		} else {
			note = b.tr("ui.pool.rechecked", back, int(docQuarantine.Hours()), still)
		}
	case "pchk":
		changes, changed, err := b.s.checkDocs(now)
		if changed {
			if err := apply(b.s, io.Discard); err != nil {
				log.Printf("bot: documents: apply: %v", err)
			}
		}
		if err != nil {
			note = b.tr("ui.pool.checkfail", html.EscapeString(err.Error()))
		} else {
			note = b.tr("ui.pool.checkdone", len(changes))
		}
	}
	unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	sc := b.poolScreen()
	sc.text = note + "\n\n" + sc.text
	return sc
}

// docReplaced tells a client its document was replaced and sends the new
// link. Like broadcast, it does not take cb.mu (the owner bot calls it).
func (cb *clientBot) docReplaced(c Client) {
	if c.Telegram == nil || !c.Active(time.Now()) {
		return
	}
	var st clientBotState
	if b, err := os.ReadFile(cb.s.clientBotStatePath()); err == nil {
		json.Unmarshal(b, &st)
	}
	l := langOf(st.Lang[c.Telegram.ID])
	if _, err := cb.t.Send(c.Telegram.ID, tr(l, "cb.doc.replaced", html.EscapeString(c.Name))); err != nil {
		log.Printf("client bot: new document for %s: %v", c.Name, err)
		return
	}
	if err := cb.sendAccess(c.Telegram.ID, c, l); err != nil {
		log.Printf("client bot: new link for %s: %v", c.Name, err)
	}
}
