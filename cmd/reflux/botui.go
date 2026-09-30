package main

import (
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"strconv"
	"strings"
	"time"

	"openflux/share"
)

// The bot's screens live in one message each: a button press edits the
// message it belongs to (home, checks, clients, a client, settings), so
// the chat does not fill up. Button data is "action" or "action:name",
// plus a time for the buttons that destroy something.

type keyboard [][]tgButton

// screen is a message's text and buttons.
type screen struct {
	text string
	kb   keyboard
}

// showKeep is how long the access data sent by QR/link stays in the chat.
var showKeep = 10 * time.Minute

// confirmFor is how long a confirmation button stays valid.
const confirmFor = 10 * time.Minute

// addFor is how long the bot waits for a new client's details.
const addFor = 10 * time.Minute

func (b *bot) tr(id string, args ...any) string { return tr(b.lang, id, args...) }

func (b *bot) btn(id, data string, args ...any) tgButton {
	return tgButton{Text: b.tr(id, args...), Data: data}
}

// botCommands fill the bot's command menu; the descriptions are messages.
var botCommands = []string{"start", "doctor", "clients", "add", "show", "pause", "resume", "expire", "revoke", "restart", "logs", "settings", "help"}

// setMenu fills the command menu in the bot's language. The caller holds
// b.mu.
func (b *bot) setMenu() {
	var cmds [][2]string
	for _, c := range botCommands {
		cmds = append(cmds, [2]string{c, b.tr("cmd." + c)})
	}
	if err := b.t.setCommands(cmds); err != nil {
		log.Printf("bot: command menu: %v", err)
	}
}

// handle answers the owner's chat; anyone else is ignored.
func (b *bot) handle(u tgUpdate) {
	if cb := u.Callback; cb != nil {
		if cb.Message != nil && cb.Message.Chat.ID == b.chat {
			b.press(cb)
		}
		return
	}
	m := u.Message
	if m == nil || m.Chat.ID != b.chat {
		return
	}
	b.mu.Lock()
	sc := b.message(strings.TrimSpace(m.Text))
	b.mu.Unlock()
	if sc.text == "" {
		return
	}
	if _, err := b.t.sendKeyboard(b.chat, sc.text, sc.kb); err != nil {
		log.Printf("bot: reply not sent: %v", err)
	}
}

// message answers a text message with a new screen (empty: nothing to
// send). The caller holds b.mu.
func (b *bot) message(text string) screen {
	f := strings.Fields(text)
	if len(f) == 0 {
		return screen{}
	}
	if !strings.HasPrefix(f[0], "/") {
		if !b.awaitAdd.IsZero() && time.Since(b.awaitAdd) < addFor {
			b.awaitAdd = time.Time{}
			return b.add(f)
		}
		return b.home()
	}
	b.awaitAdd = time.Time{}
	cmd, _, _ := strings.Cut(f[0], "@") // /status@SomeBot
	args := f[1:]
	one := func(then func(string) screen) screen {
		if len(args) != 1 {
			return screen{text: b.tr("ui.usage." + strings.TrimPrefix(cmd, "/"))}
		}
		return then(args[0])
	}
	switch cmd {
	case "/start", "/menu", "/status", "/list":
		return b.home()
	case "/doctor":
		return b.doctorScreen()
	case "/clients":
		return b.clientsScreen()
	case "/help":
		return b.help()
	case "/settings", "/lang":
		return b.settingsScreen()
	case "/add":
		if len(args) == 0 {
			return b.addPrompt()
		}
		return b.add(args)
	case "/show":
		return one(func(n string) screen { b.show(n); return screen{} })
	case "/pause":
		return one(func(n string) screen { return b.setPaused(n, true) })
	case "/resume":
		return one(func(n string) screen { return b.setPaused(n, false) })
	case "/expire":
		if len(args) != 2 {
			return screen{text: b.tr("ui.usage.expire")}
		}
		return b.expire(args[0], args[1])
	case "/revoke":
		return one(b.revokeAsk)
	case "/restart":
		return b.restartAsk()
	case "/logs":
		return one(func(n string) screen { return screen{text: b.logs(n)} })
	}
	return screen{text: b.tr("ui.unknown")}
}

// press handles a button: it edits the screen it belongs to.
func (b *bot) press(cb *tgCallback) {
	action, arg, _ := strings.Cut(cb.Data, ":")
	b.mu.Lock()
	sc, toast := b.button(action, arg)
	b.mu.Unlock()
	b.t.answer(cb.ID, toast)
	if sc.text == "" {
		return
	}
	if err := b.t.editKeyboard(b.chat, cb.Message.MessageID, sc.text, sc.kb); err != nil && !isNotModified(err) {
		log.Printf("bot: %v", err)
	}
}

// button runs a button's action and returns the screen to show in its
// message (empty: leave it) and a short notice. The caller holds b.mu.
func (b *bot) button(action, arg string) (screen, string) {
	switch action {
	case "home":
		b.awaitAdd = time.Time{}
		return b.home(), ""
	case "doc":
		return b.doctorScreen(), ""
	case "cls":
		return b.clientsScreen(), ""
	case "c":
		return b.clientScreen(arg), ""
	case "qr":
		b.show(arg)
		return screen{}, b.tr("ui.sent")
	case "lg":
		if _, err := b.t.send(b.chat, b.logs(arg)); err != nil {
			log.Printf("bot: logs: %v", err)
		}
		return screen{}, ""
	case "pa":
		return b.setPaused(arg, true), ""
	case "re":
		return b.setPaused(arg, false), ""
	case "ex30":
		return b.extend(arg), ""
	case "exn":
		return b.expire(arg, "never"), ""
	case "rv":
		return b.revokeAsk(arg), ""
	case "rv!":
		name, at, _ := strings.Cut(arg, ":")
		if !fresh(at) {
			return b.clientScreen(name), b.tr("ui.expired.button")
		}
		return b.revoke(name), ""
	case "rs":
		return b.restartAsk(), ""
	case "rs!":
		if !fresh(arg) {
			return b.home(), b.tr("ui.expired.button")
		}
		return b.restart(), ""
	case "add":
		return b.addPrompt(), ""
	case "set":
		return b.settingsScreen(), ""
	case "lang":
		return b.setLang(lang(arg)), ""
	}
	return b.home(), ""
}

// stamp and fresh put a time into a confirmation button: the screen is
// an edited message, whose own date is when it was first sent.
func stamp() string { return strconv.FormatInt(time.Now().Unix(), 10) }

func fresh(s string) bool {
	at, err := strconv.ParseInt(s, 10, 64)
	return err == nil && time.Since(time.Unix(at, 0)) < confirmFor
}

// isNotModified: Telegram refuses an edit that changes nothing (Refresh
// with nothing new).
func isNotModified(err error) bool {
	var te *tgError
	return errors.As(err, &te) && strings.Contains(te.Desc, "not modified")
}

// ---- screens ----

// clientView is what the screens show about a client.
type clientView struct {
	c       Client
	active  bool
	running bool
	status  *statusView
}

type statusView struct {
	online    bool
	up        time.Duration
	down, upB uint64
}

func (b *bot) clientViews(clients []Client) []clientView {
	now := time.Now()
	states := containerStates()
	live := nodeStatuses(b.s, activeClients(clients, now))
	var out []clientView
	for _, c := range clients {
		v := clientView{c: c, active: c.Active(now), running: states["reflux-node-"+c.Name] != ""}
		if st, ok := live[c.Name]; ok {
			v.status = &statusView{online: st.Connected, up: time.Duration(st.UptimeMs) * time.Millisecond, down: st.BytesOut, upB: st.BytesIn}
		}
		out = append(out, v)
	}
	return out
}

// mark is a client's state at a glance.
func (v clientView) mark() string {
	switch {
	case v.c.Paused:
		return "⏸"
	case !v.active:
		return "⌛"
	case !v.running:
		return "🔴"
	case v.status == nil:
		return "🟡"
	case v.status.online:
		return "🟢"
	}
	return "⚪"
}

// state is a client's state in words.
func (b *bot) state(v clientView) string {
	switch {
	case !v.active:
		p := accessPhrase(v.c, time.Now())
		return b.tr(p.id, p.args...)
	case !v.running:
		return b.tr("ui.node.down")
	case v.status == nil:
		return b.tr("ui.nostatus")
	}
	s := b.tr("offline")
	if v.status.online {
		s = b.tr("online")
	}
	return s + " · ↓" + humanBytes(v.status.down) + " ↑" + humanBytes(v.status.upB)
}

func (b *bot) home() screen {
	var t strings.Builder
	t.WriteString("🛰 <b>ReFlux</b>\n")
	if st, err := readEgressStatus(); err != nil {
		t.WriteString(b.tr("ui.egress.none") + "\n")
	} else {
		world := "✅ " + html.EscapeString(st.World)
		if !st.WorldOK {
			world = "❌ " + html.EscapeString(st.World)
		}
		russia := "✅ " + b.tr(ruMode(st).id)
		switch {
		case !st.RUOK:
			russia = "❌ " + b.tr(ruMode(st).id)
		case st.RUFallback:
			russia = "⚠️ " + b.tr(ruMode(st).id)
		}
		t.WriteString(b.tr("ui.world", world) + "\n" + b.tr("ui.russia", russia) + "\n" + b.tr("ui.killswitch", st.Dropped) + "\n")
	}
	clients, err := b.s.List()
	t.WriteString("\n" + b.tr("ui.clients") + "\n")
	switch {
	case err != nil:
		t.WriteString(html.EscapeString(err.Error()) + "\n")
	case len(clients) == 0:
		t.WriteString(b.tr("ui.clients.none") + "\n")
	}
	for _, v := range b.clientViews(clients) {
		fmt.Fprintf(&t, "%s <b>%s</b> · %s\n", v.mark(), html.EscapeString(v.c.Name), b.state(v))
	}
	t.WriteString("\n" + b.tr("ui.updated", time.Now().Format("15:04:05")))
	return screen{t.String(), keyboard{
		{b.btn("b.refresh", "home"), b.btn("b.doctor", "doc")},
		{b.btn("b.clients", "cls"), b.btn("b.add", "add")},
		{b.btn("b.settings", "set")},
	}}
}

func (b *bot) doctorScreen() screen {
	fs := runChecks(b.s)
	warns, fails := count(fs)
	var t strings.Builder
	t.WriteString(b.tr("ui.doctor.title") + "\n\n")
	for _, f := range fs {
		t.WriteString(alertLine(f, b.lang) + "\n")
	}
	t.WriteString("\n" + b.tr("ui.doctor.sum", fails, warns) + "\n" + b.tr("ui.updated", time.Now().Format("15:04:05")))
	return screen{t.String(), keyboard{
		{b.btn("b.refresh", "doc"), b.btn("b.restart", "rs")},
		{b.btn("b.home", "home")},
	}}
}

func (b *bot) clientsScreen() screen {
	clients, err := b.s.List()
	if err != nil {
		return screen{html.EscapeString(err.Error()), keyboard{{b.btn("b.home", "home")}}}
	}
	kb := keyboard{}
	var row []tgButton
	for _, v := range b.clientViews(clients) {
		row = append(row, tgButton{Text: v.mark() + " " + v.c.Name, Data: "c:" + v.c.Name})
		if len(row) == 2 {
			kb, row = append(kb, row), nil
		}
	}
	if row != nil {
		kb = append(kb, row)
	}
	kb = append(kb, []tgButton{b.btn("b.add", "add"), b.btn("b.home", "home")})
	text := b.tr("ui.clients.title", len(clients)) + "\n" + b.tr("ui.clients.hint")
	if len(clients) == 0 {
		text = b.tr("ui.clients.title", 0) + "\n" + b.tr("ui.clients.none")
	}
	return screen{text, kb}
}

func (b *bot) clientScreen(name string) screen {
	c, err := b.s.Get(name)
	if err != nil {
		return screen{html.EscapeString(err.Error()), keyboard{{b.btn("b.clients", "cls")}}}
	}
	v := b.clientViews([]Client{c})[0]
	now := time.Now()
	p := accessPhrase(c, now)
	var t strings.Builder
	fmt.Fprintf(&t, "%s <b>%s</b>\n", v.mark(), html.EscapeString(c.Name))
	t.WriteString(b.tr("ui.client.access", b.tr(p.id, p.args...)) + "\n")
	switch {
	case !v.active:
		t.WriteString(b.tr("ui.client.node", b.tr("ui.node.stopped")) + "\n")
	case !v.running:
		t.WriteString(b.tr("ui.client.node", b.tr("ui.node.down")) + "\n")
	case v.status == nil:
		t.WriteString(b.tr("ui.client.node", b.tr("ui.nostatus")) + "\n")
	default:
		t.WriteString(b.tr("ui.client.node", b.tr("ui.node.up", durationIn(b.lang, v.status.up))) + "\n")
		online := b.tr("offline")
		if v.status.online {
			online = b.tr("online")
		}
		t.WriteString(b.tr("ui.client.client", online) + "\n")
		t.WriteString(b.tr("ui.client.traffic", humanBytes(v.status.down), humanBytes(v.status.upB)) + "\n")
	}
	t.WriteString(b.tr("ui.client.created", c.Created.Local().Format(time.DateOnly)))
	toggle := b.btn("b.pause", "pa:"+c.Name)
	if c.Paused {
		toggle = b.btn("b.resume", "re:"+c.Name)
	}
	return screen{t.String(), keyboard{
		{b.btn("b.qr", "qr:"+c.Name), toggle},
		{b.btn("b.plus30", "ex30:"+c.Name), b.btn("b.never", "exn:"+c.Name)},
		{b.btn("b.logs", "lg:"+c.Name), b.btn("b.revoke", "rv:"+c.Name)},
		{b.btn("b.clients", "cls"), b.btn("b.home", "home")},
	}}
}

func (b *bot) settingsScreen() screen {
	return screen{b.tr("ui.settings", b.tr("lang.name")), keyboard{
		{{Text: "🇷🇺 Русский", Data: "lang:ru"}, {Text: "🇬🇧 English", Data: "lang:en"}},
		{b.btn("b.home", "home")},
	}}
}

func (b *bot) help() screen {
	var t strings.Builder
	t.WriteString(b.tr("ui.help") + "\n\n")
	for _, c := range botCommands {
		fmt.Fprintf(&t, "/%s — %s\n", c, html.EscapeString(b.tr("cmd."+c)))
	}
	return screen{t.String(), keyboard{{b.btn("b.home", "home")}}}
}

// ---- actions ----

// failed shows an error on screen, with a way back.
func (b *bot) failed(err error, back tgButton) screen {
	return screen{"⚠️ " + html.EscapeString(err.Error()), keyboard{{back, b.btn("b.home", "home")}}}
}

func (b *bot) setPaused(name string, paused bool) screen {
	err := b.change(func() error {
		return setAccess(b.s, name, io.Discard, func(c *Client) error {
			if !paused && !c.Expires.IsZero() && !time.Now().Before(c.Expires) {
				return errors.New(b.tr("ui.resume.expired"))
			}
			c.Paused = paused
			return nil
		})
	})
	if err != nil {
		return b.failed(err, b.btn("b.back", "c:"+name))
	}
	return b.clientScreen(name)
}

func (b *bot) expire(name, when string) screen {
	t, err := parseExpiry(when, time.Now())
	if err == nil {
		err = b.change(func() error {
			return setAccess(b.s, name, io.Discard, func(c *Client) error { c.Expires = t; return nil })
		})
	}
	if err != nil {
		return b.failed(err, b.btn("b.back", "c:"+name))
	}
	return b.clientScreen(name)
}

// extend adds 30 days to the access: from its end while it lasts, from
// now once it ended. Unlimited access stays unlimited.
func (b *bot) extend(name string) screen {
	err := b.change(func() error {
		return setAccess(b.s, name, io.Discard, func(c *Client) error {
			if c.Expires.IsZero() {
				return nil
			}
			from := time.Now()
			if c.Expires.After(from) {
				from = c.Expires
			}
			c.Expires = from.AddDate(0, 0, 30)
			return nil
		})
	})
	if err != nil {
		return b.failed(err, b.btn("b.back", "c:"+name))
	}
	return b.clientScreen(name)
}

func (b *bot) addPrompt() screen {
	b.awaitAdd = time.Now()
	return screen{b.tr("ui.add.prompt"), keyboard{{b.btn("b.cancel", "home")}}}
}

// add creates a client from "<name> <document-url> [expiry]".
func (b *bot) add(args []string) screen {
	if len(args) < 2 || len(args) > 3 {
		return screen{b.tr("ui.add.prompt"), keyboard{{b.btn("b.home", "home")}}}
	}
	expires := "never"
	if len(args) == 3 {
		expires = args[2]
	}
	when, err := parseExpiry(expires, time.Now())
	if err != nil {
		return b.failed(err, b.btn("b.add", "add"))
	}
	var c Client
	err = b.change(func() error {
		var err error
		if c, err = b.s.Add(args[0], "mailru", args[1]); err != nil {
			return err
		}
		if !when.IsZero() {
			c.Expires = when
			if err := b.s.Save(c); err != nil {
				return err
			}
		}
		if err := apply(b.s, io.Discard); err != nil {
			return fmt.Errorf("%s: %w", b.tr("ui.add.nostart", c.Name), err)
		}
		return nil
	})
	if err != nil {
		return b.failed(err, b.btn("b.add", "add"))
	}
	sc := b.clientScreen(c.Name)
	sc.text = b.tr("ui.add.done", html.EscapeString(c.Name)) + "\n\n" + sc.text
	return sc
}

func (b *bot) revokeAsk(name string) screen {
	if _, err := b.s.Get(name); err != nil {
		return b.failed(err, b.btn("b.clients", "cls"))
	}
	return screen{b.tr("ui.revoke.ask", html.EscapeString(name)), keyboard{
		{b.btn("b.confirm.revoke", "rv!:"+name+":"+stamp(), name)},
		{b.btn("b.cancel", "c:"+name)},
	}}
}

func (b *bot) revoke(name string) screen {
	err := b.change(func() error { return cmdRevoke(b.s, []string{name, "--yes"}, nil, io.Discard) })
	if err != nil {
		return b.failed(err, b.btn("b.clients", "cls"))
	}
	sc := b.clientsScreen()
	sc.text = b.tr("ui.revoke.done", html.EscapeString(name)) + "\n\n" + sc.text
	return sc
}

func (b *bot) restartAsk() screen {
	return screen{b.tr("ui.restart.ask"), keyboard{
		{b.btn("b.confirm.restart", "rs!:"+stamp())},
		{b.btn("b.cancel", "home")},
	}}
}

func (b *bot) restart() screen {
	if err := b.change(func() error { return cmdRestart(b.s, io.Discard) }); err != nil {
		return b.failed(err, b.btn("b.doctor", "doc"))
	}
	sc := b.home()
	sc.text = b.tr("ui.restart.done") + "\n\n" + sc.text
	return sc
}

func (b *bot) setLang(l lang) screen {
	if l != langEN {
		l = langRU
	}
	b.lang = l
	c, err := b.s.loadBotConfig()
	if err == nil {
		c.Lang = string(l)
		err = b.s.saveBotConfig(c)
	}
	if err != nil {
		log.Printf("bot: saving the language: %v", err)
	}
	b.setMenu()
	return b.settingsScreen()
}

// show sends what a client's app needs: the QR code and the fields for
// manual entry. Both are secrets, so they are deleted after showKeep.
func (b *bot) show(name string) {
	fail := func(err error) { b.t.send(b.chat, "⚠️ "+html.EscapeString(err.Error())) }
	c, err := b.s.Get(name)
	if err != nil {
		fail(err)
		return
	}
	key, err := b.s.Key(c.Name)
	if err != nil {
		fail(err)
		return
	}
	link, err := share.MakeLink(shareConfig(c, key))
	if err != nil {
		fail(err)
		return
	}
	png, err := share.PNG(link, 512)
	if err != nil {
		fail(err)
		return
	}
	e := html.EscapeString
	mins := int(showKeep.Minutes())
	photo, err := b.t.sendPhoto(b.chat, png, b.tr("ui.show.photo", e(c.Name), mins))
	if err != nil {
		log.Printf("bot: show: %v", err)
		fail(errors.New(b.tr("ui.show.failed")))
		return
	}
	p := accessPhrase(c, time.Now())
	text, err := b.t.send(b.chat, b.tr("ui.show.text", e(c.Name), mins, b.tr(p.id, p.args...),
		e(c.Transport), e(c.URL), e(key), e(link)))
	if err != nil {
		log.Printf("bot: show: %v", err)
	}
	time.AfterFunc(showKeep, func() {
		for _, id := range []int64{photo, text} {
			if id != 0 {
				if err := b.t.deleteMessage(b.chat, id); err != nil {
					log.Printf("bot: deleting access data: %v", err)
				}
			}
		}
	})
}

// logs returns the tail of a node's or the egress's log as a message.
func (b *bot) logs(name string) string {
	container := "reflux-egress"
	if name != "egress" {
		if err := validName(name); err != nil {
			return "⚠️ " + html.EscapeString(err.Error())
		}
		container = "reflux-node-" + name
	}
	var out strings.Builder
	old := dockerStderr
	dockerStderr = &out // the core and the egress log to stderr
	err := runDocker(&out, "logs", "--tail", "40", container)
	dockerStderr = old
	text := out.String()
	if err != nil {
		text += "\nerror: " + err.Error()
	}
	return "📜 <b>" + html.EscapeString(name) + "</b>\n" + preTail(text)
}
