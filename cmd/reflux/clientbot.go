package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/p1neappleXpress/OpenFlux/share"
)

// The client bot: the public bot people use to ask for a channel and to
// get what their app needs. It runs next to the owner bot in `reflux bot
// run` when client-bot.json has a token, answers private chats only, and
// gives a person nothing but their own channel. Requests go through
// requests.go; whatever approves one (the owner bot, the panel, the
// CLI), this bot sends the person the link.

const (
	clientAskFor   = 10 * time.Minute // the note to a request, after "ask"
	clientQREvery  = 2 * time.Minute  // the link and QR again at most this often
	clientRemind   = 3 * 24 * time.Hour
	clientDeliver  = 20 * time.Second
	clientEndedFor = 3 * 24 * time.Hour // an expiry this recent is announced
)

type clientBotConfig struct {
	Token string `json:"token"`
}

func (s Store) clientBotPath() string      { return filepath.Join(s.Root, "client-bot.json") }
func (s Store) clientBotStatePath() string { return filepath.Join(s.Root, "client-bot-state.json") }
func (s Store) clientHelpPath() string     { return filepath.Join(s.Root, "client-help.txt") }

func (s Store) loadClientBot() (clientBotConfig, bool, error) {
	var c clientBotConfig
	b, err := os.ReadFile(s.clientBotPath())
	if errors.Is(err, os.ErrNotExist) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	return c, true, json.Unmarshal(b, &c)
}

// clientBotState is what the bot remembers across restarts.
type clientBotState struct {
	Lang     map[int64]string     `json:"lang,omitempty"`     // a person's language code
	Reminded map[string]time.Time `json:"reminded,omitempty"` // client → the expiry warned of
	Ended    map[string]time.Time `json:"ended,omitempty"`    // client → the expiry announced
}

type clientBot struct {
	s     Store
	t     *telegram
	owner *bot // told of new requests at once; nil without the owner bot
	mu    sync.Mutex
	// asking: people writing a note to their request; qrAt: when each
	// last got the link.
	asking map[int64]time.Time
	qrAt   map[int64]time.Time
	st     clientBotState
}

func newClientBot(s Store, c clientBotConfig, owner *bot) *clientBot {
	cb := &clientBot{s: s, t: newTelegram(c.Token), owner: owner,
		asking: map[int64]time.Time{}, qrAt: map[int64]time.Time{}}
	if b, err := os.ReadFile(s.clientBotStatePath()); err == nil {
		json.Unmarshal(b, &cb.st)
	}
	if cb.st.Lang == nil {
		cb.st.Lang = map[int64]string{}
	}
	if cb.st.Reminded == nil {
		cb.st.Reminded = map[string]time.Time{}
	}
	if cb.st.Ended == nil {
		cb.st.Ended = map[string]time.Time{}
	}
	return cb
}

func (cb *clientBot) save() {
	if err := writeJSON(cb.s.clientBotStatePath(), cb.st); err != nil {
		log.Printf("client bot: %v", err)
	}
}

// langOf is a person's language: English for en, Russian otherwise.
func langOf(code string) lang {
	if strings.HasPrefix(code, "en") {
		return langEN
	}
	return langRU
}

// langFor is the language a person last used with the bot.
func (cb *clientBot) langFor(id int64) lang { return langOf(cb.st.Lang[id]) }

func (cb *clientBot) btn(l lang, id, data string, a ...any) tgButton {
	return tgButton{Text: tr(l, id, a...), Data: data}
}

// run answers people until the process stops; delivering runs alongside.
func (cb *clientBot) run() error {
	offset, err := skipPending(cb.t)
	if err != nil {
		return err
	}
	cb.t.setCommands([][2]string{{"start", tr(langRU, "cbcmd.start")}, {"qr", tr(langRU, "cbcmd.qr")}, {"help", tr(langRU, "cbcmd.help")}})
	go func() {
		for {
			cb.mu.Lock()
			cb.deliver(time.Now())
			cb.mu.Unlock()
			time.Sleep(clientDeliver)
		}
	}()
	log.Print("client bot: started")
	for {
		ups, err := cb.t.getUpdates(offset, 50*time.Second)
		if err != nil {
			log.Printf("client bot: %v", err)
			time.Sleep(10 * time.Second)
			continue
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			cb.handle(u)
		}
	}
}

// handle answers one update from a private chat.
func (cb *clientBot) handle(u tgUpdate) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if q := u.Callback; q != nil {
		if q.From == nil || q.Message == nil || q.Message.Chat.Type != "private" {
			return
		}
		cb.seen(q.From)
		sc := cb.press(q.From, q.Data)
		cb.t.answer(q.ID, "")
		if sc.text != "" {
			if err := cb.t.editKeyboard(q.Message.Chat.ID, q.Message.MessageID, sc.text, sc.kb); err != nil && !isNotModified(err) {
				log.Printf("client bot: %v", err)
			}
		}
		return
	}
	m := u.Message
	if m == nil || m.From == nil || m.Chat.Type != "private" {
		return
	}
	cb.seen(m.From)
	sc := cb.message(m.From, strings.TrimSpace(m.Text))
	if sc.text != "" {
		cb.t.sendKeyboard(m.Chat.ID, sc.text, sc.kb)
	}
}

// seen remembers a person's language for the messages the bot starts.
func (cb *clientBot) seen(u *tgUser) {
	if u.LanguageCode != "" && cb.st.Lang[u.ID] != u.LanguageCode {
		cb.st.Lang[u.ID] = u.LanguageCode
		cb.save()
	}
}

func (cb *clientBot) message(u *tgUser, text string) screen {
	l := langOf(u.LanguageCode)
	if at, ok := cb.asking[u.ID]; ok && !strings.HasPrefix(text, "/") {
		delete(cb.asking, u.ID)
		if time.Since(at) < clientAskFor {
			return cb.ask(u, text)
		}
	}
	switch strings.SplitN(text, "@", 2)[0] {
	case "/qr":
		return cb.access(u)
	case "/help":
		return cb.help(l)
	}
	return cb.home(u)
}

func (cb *clientBot) press(u *tgUser, data string) screen {
	l := langOf(u.LanguageCode)
	switch data {
	case "home":
		delete(cb.asking, u.ID)
		return cb.home(u)
	case "ask":
		cb.asking[u.ID] = time.Now()
		return screen{tr(l, "cb.ask.note"), keyboard{{cb.btn(l, "b.cb.nonote", "ask!")}, {cb.btn(l, "b.cancel", "home")}}}
	case "ask!":
		delete(cb.asking, u.ID)
		return cb.ask(u, "")
	case "wd":
		if r, err := cb.s.getRequest(u.ID); err == nil && r.State == reqPending {
			cb.s.removeRequest(u.ID)
		}
		return cb.home(u)
	case "qr":
		sc := cb.access(u)
		if sc.text == "" {
			return cb.home(u)
		}
		return sc
	case "help":
		return cb.help(l)
	case "ext":
		return cb.extend(u)
	}
	return cb.home(u)
}

// home is what a person sees: their channel, their request, or how to ask.
func (cb *clientBot) home(u *tgUser) screen {
	l := langOf(u.LanguageCode)
	e := html.EscapeString
	r, rerr := cb.s.getRequest(u.ID)
	if rerr == nil && r.State == reqBlocked {
		return screen{tr(l, "cb.closed"), nil}
	}
	if c, ok := cb.s.clientOf(u.ID); ok {
		now := time.Now()
		p := accessPhrase(c, now)
		state := tr(l, "cb.state.down")
		if c.Active(now) {
			if st, ok := nodeStatuses(cb.s, []Client{c})[c.Name]; ok {
				state = tr(l, "offline")
				if st.Connected {
					state = tr(l, "online")
				}
			}
		}
		text := tr(l, "cb.channel", e(c.Name), tr(l, p.id, p.args...), state)
		if _, month, ok := cb.s.trafficNow(c.Name, now); ok {
			text += "\n" + tr(l, "cb.traffic", humanBytes(month.Down), humanBytes(month.Up))
		}
		kb := keyboard{{cb.btn(l, "b.cb.qr", "qr"), cb.btn(l, "b.cb.help", "help")}}
		switch {
		case rerr == nil && r.State == reqPending:
			text += "\n\n" + tr(l, "cb.extend.waiting")
		case !c.Expires.IsZero() && c.Expires.Sub(now) < 7*24*time.Hour:
			kb = append(kb, []tgButton{cb.btn(l, "b.cb.extend", "ext")})
		}
		return screen{text, append(kb, []tgButton{cb.btn(l, "b.refresh", "home")})}
	}
	if rerr == nil {
		switch r.State {
		case reqPending, reqApproved:
			return screen{tr(l, "cb.waiting"), keyboard{{cb.btn(l, "b.refresh", "home"), cb.btn(l, "b.cb.withdraw", "wd")}}}
		case reqRejected:
			if again := r.Decided.Add(askAgainFrom); time.Now().Before(again) {
				return screen{tr(l, "cb.rejected", again.Local().Format("02.01 15:04")), nil}
			}
		}
	}
	return screen{tr(l, "cb.intro"), keyboard{{cb.btn(l, "b.cb.ask", "ask")}, {cb.btn(l, "b.cb.help", "help")}}}
}

// ask files a request for a channel, and tells the owner at once.
func (cb *clientBot) ask(u *tgUser, text string) screen {
	l := langOf(u.LanguageCode)
	_, err := cb.s.newRequest(*u, reqAccess, text, time.Now())
	switch {
	case errors.Is(err, errTooMany):
		return screen{tr(l, "cb.full"), keyboard{{cb.btn(l, "b.refresh", "home")}}}
	case err != nil:
		return cb.home(u)
	}
	cb.tellOwner()
	return screen{tr(l, "cb.sent"), keyboard{{cb.btn(l, "b.refresh", "home"), cb.btn(l, "b.cb.withdraw", "wd")}}}
}

// extend asks the owner for more time on the person's channel.
func (cb *clientBot) extend(u *tgUser) screen {
	l := langOf(u.LanguageCode)
	if _, err := cb.s.newRequest(*u, reqExtend, "", time.Now()); err != nil {
		return cb.home(u)
	}
	cb.tellOwner()
	sc := cb.home(u)
	sc.text = tr(l, "cb.extend.sent") + "\n\n" + sc.text
	return sc
}

func (cb *clientBot) tellOwner() {
	if cb.owner == nil {
		return
	}
	cb.owner.mu.Lock()
	cb.owner.notifyRequests()
	cb.owner.mu.Unlock()
}

// access sends a person their channel's link and QR, deleted after
// showKeep; at most every clientQREvery. The screen says why not, or is
// empty when they went out.
func (cb *clientBot) access(u *tgUser) screen {
	l := langOf(u.LanguageCode)
	c, ok := cb.s.clientOf(u.ID)
	if !ok {
		return cb.home(u)
	}
	now := time.Now()
	if !c.Active(now) {
		p := accessPhrase(c, now)
		return screen{tr(l, "cb.qr.off", tr(l, p.id, p.args...)), keyboard{{cb.btn(l, "b.refresh", "home")}}}
	}
	if at, ok := cb.qrAt[u.ID]; ok && now.Sub(at) < clientQREvery {
		return screen{tr(l, "cb.qr.wait", int(clientQREvery.Minutes())), keyboard{{cb.btn(l, "b.refresh", "home")}}}
	}
	if err := cb.sendAccess(u.ID, c, l); err != nil {
		log.Printf("client bot: access for %s: %v", c.Name, err)
		return screen{tr(l, "ui.show.failed"), keyboard{{cb.btn(l, "b.refresh", "home")}}}
	}
	cb.qrAt[u.ID] = now
	return screen{}
}

// sendAccess sends a channel's QR and fields to its person's chat.
func (cb *clientBot) sendAccess(chat int64, c Client, l lang) error {
	key, err := cb.s.Key(c.Name)
	if err != nil {
		return err
	}
	link, err := share.MakeLink(shareConfig(c, key))
	if err != nil {
		return err
	}
	png, err := share.PNG(link, 512)
	if err != nil {
		return err
	}
	e := html.EscapeString
	mins := int(showKeep.Minutes())
	photo, err := cb.t.sendPhoto(chat, png, tr(l, "cb.photo", mins))
	if err != nil {
		return err
	}
	var text string
	if c.session() {
		var docs strings.Builder
		for i, d := range c.Docs() {
			docs.WriteString(tr(l, "ui.show.doc", i+1, e(d.Transport), docPriority(i), e(d.URL)) + "\n")
		}
		text = tr(l, "cb.show.session", e(c.Name), mins, docs.String(), e(c.context()), e(key), e(link))
	} else {
		text = tr(l, "cb.show.text", e(c.Name), mins, e(c.Transport), e(c.URL), e(key), e(link))
	}
	msg, err := cb.t.send(chat, text)
	time.AfterFunc(showKeep, func() {
		for _, id := range []int64{photo, msg} {
			if id != 0 {
				cb.t.deleteMessage(chat, id)
			}
		}
	})
	return err
}

// help tells how to connect, with the owner's own words from
// client-help.txt when there are any.
func (cb *clientBot) help(l lang) screen {
	text := tr(l, "cb.help")
	if b, err := os.ReadFile(cb.s.clientHelpPath()); err == nil && strings.TrimSpace(string(b)) != "" {
		text += "\n\n" + html.EscapeString(strings.TrimSpace(string(b)))
	}
	return screen{text, keyboard{{cb.btn(l, "b.back", "home")}}}
}

// deliver tells people what was decided — the link for an approved
// request, the new date for an extension, a rejection — and reminds them
// of access ending. The caller holds cb.mu.
func (cb *clientBot) deliver(now time.Time) {
	reqs, _ := cb.s.listRequests()
	for _, r := range reqs {
		l := langOf(r.Lang)
		if code := cb.st.Lang[r.ID]; code != "" {
			l = langOf(code)
		}
		switch {
		case r.State == reqApproved:
			c, err := cb.s.Get(r.Client)
			if err != nil {
				cb.s.removeRequest(r.ID)
				continue
			}
			p := accessPhrase(c, now)
			if r.Kind == reqExtend {
				_, err = cb.t.send(r.ID, tr(l, "cb.extended", html.EscapeString(c.Name), tr(l, p.id, p.args...)))
			} else if _, err = cb.t.sendKeyboard(r.ID, tr(l, "cb.welcome", html.EscapeString(c.Name), tr(l, p.id, p.args...)),
				keyboard{{cb.btn(l, "b.cb.help", "help"), cb.btn(l, "b.refresh", "home")}}); err == nil {
				err = cb.sendAccess(r.ID, c, l)
				cb.qrAt[r.ID] = now
			}
			if err == nil || unreachable(err) {
				cb.s.removeRequest(r.ID)
			} else {
				log.Printf("client bot: telling %d: %v", r.ID, err)
			}
		case r.State == reqRejected && !r.Told:
			if _, err := cb.t.send(r.ID, tr(l, "cb.rejected.now")); err == nil || unreachable(err) {
				r.Told = true
				cb.s.saveRequest(r)
			}
		}
	}
	cb.remind(now)
}

// unreachable: the person blocked the bot or deleted the chat; telling
// them again will not work either.
func unreachable(err error) bool {
	var te *tgError
	return errors.As(err, &te) && (te.Code == 403 || te.Code == 400)
}

// remind tells people their access ends in clientRemind, and when it has
// ended; once per expiry date.
func (cb *clientBot) remind(now time.Time) {
	clients, _ := cb.s.List()
	changed := false
	for _, c := range clients {
		if c.Telegram == nil || c.Expires.IsZero() || c.Paused {
			continue
		}
		l, left := cb.langFor(c.Telegram.ID), c.Expires.Sub(now)
		ext := keyboard{{cb.btn(l, "b.cb.extend", "ext")}}
		switch {
		case left > 0 && left < clientRemind && !cb.st.Reminded[c.Name].Equal(c.Expires):
			text := tr(l, "cb.remind", html.EscapeString(c.Name), c.Expires.Local().Format("02.01 15:04"), durationIn(l, left))
			if _, err := cb.t.sendKeyboard(c.Telegram.ID, text, ext); err == nil || unreachable(err) {
				cb.st.Reminded[c.Name], changed = c.Expires, true
			}
		case left <= 0 && -left < clientEndedFor && !cb.st.Ended[c.Name].Equal(c.Expires):
			if _, err := cb.t.sendKeyboard(c.Telegram.ID, tr(l, "cb.ended", html.EscapeString(c.Name)), ext); err == nil || unreachable(err) {
				cb.st.Ended[c.Name], changed = c.Expires, true
			}
		}
	}
	if changed {
		cb.save()
	}
}

// ---- setup ----

// clientBotSetup links the client bot: a token from @BotFather, checked.
func clientBotSetup(s Store, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 1 && args[0] == "off" {
		if err := ignoreMissing(os.Remove(s.clientBotPath())); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "The client bot is off; restart the bot: reflux bot install")
		return nil
	}
	if len(args) != 0 {
		return errors.New(botUsage)
	}
	fmt.Fprint(stdout, `1. In Telegram, open @BotFather, send /newbot and make a second bot for your clients.
2. Paste its token here (it is kept in `+s.clientBotPath()+`):
> `)
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	token := strings.TrimSpace(line)
	if !tokenRe.MatchString(token) {
		return errors.New("that does not look like a bot token (digits:letters)")
	}
	if c, err := s.loadBotConfig(); err == nil && c.Token == token {
		return errors.New("that is your own bot's token: the client bot needs a bot of its own")
	}
	me, err := newTelegram(token).getMe()
	if err != nil {
		return fmt.Errorf("the token does not work: %w", err)
	}
	if err := writeJSON(s.clientBotPath(), clientBotConfig{Token: token}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Linked @%s. Give people https://t.me/%s; restart the bot to start it: reflux bot install\n", me.Username, me.Username)
	return nil
}
