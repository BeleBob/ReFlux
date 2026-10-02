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

	"github.com/p1neappleXpress/OpenFlux/share"
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

// awaitFor is how long the bot waits for the answer to its question.
const awaitFor = 10 * time.Minute

func (b *bot) tr(id string, args ...any) string { return tr(b.lang, id, args...) }

func (b *bot) btn(id, data string, args ...any) tgButton {
	return tgButton{Text: b.tr(id, args...), Data: data}
}

// botCommands fill the bot's command menu; the descriptions are messages.
var botCommands = []string{"start", "doctor", "clients", "add", "show", "docs", "pause", "resume", "expire", "rename", "revoke", "restart", "logs", "server", "speedtest", "gateway", "report", "requests", "web", "settings", "help"}

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
		q := b.await
		b.await = awaiting{}
		if q.kind == "" || time.Since(q.at) > awaitFor {
			return b.home()
		}
		switch q.kind {
		case "add":
			return b.add(f)
		case "rename":
			return b.rename(q.name, f[0])
		case "expire":
			return b.expireTo(q.name, f[0])
		case "doc":
			return b.addDoc(q.name, &Doc{Transport: transportOf(f[0]), URL: f[0]})
		}
		return b.home()
	}
	b.await = awaiting{}
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
	case "/report":
		return b.reportScreen()
	case "/requests":
		return b.requestsScreen()
	case "/settings", "/lang":
		return b.settingsScreen()
	case "/add":
		if len(args) == 0 {
			return b.addPrompt()
		}
		return b.add(args)
	case "/show":
		return one(func(n string) screen { b.show(n); return screen{} })
	case "/docs":
		return one(b.docsScreen)
	case "/pause":
		return one(func(n string) screen { return b.setPaused(n, true) })
	case "/resume":
		return one(func(n string) screen { return b.setPaused(n, false) })
	case "/expire":
		if len(args) != 2 {
			return screen{text: b.tr("ui.usage.expire")}
		}
		return b.expireTo(args[0], args[1])
	case "/rename":
		if len(args) != 2 {
			return screen{text: b.tr("ui.usage.rename")}
		}
		return b.rename(args[0], args[1])
	case "/speedtest":
		return b.speed()
	case "/gateway":
		return b.gatewayScreen()
	case "/server":
		return b.serverScreen()
	case "/web":
		link, err := b.s.loginLink()
		if err != nil {
			return screen{text: b.tr("ui.web.none")}
		}
		return screen{text: b.tr("ui.web.link", int(loginFor.Minutes()), html.EscapeString(link))}
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
	if action == "sp" {
		// The test takes a while: say so before the spinner times out.
		b.t.answer(cb.ID, b.tr("ui.speed.wait"))
		b.mu.Lock()
		sc := b.speed()
		b.mu.Unlock()
		if _, err := b.t.sendKeyboard(b.chat, sc.text, sc.kb); err != nil {
			log.Printf("bot: speed test: %v", err)
		}
		return
	}
	b.mu.Lock()
	if action == "tgme" && cb.From != nil {
		b.linkTo = cb.From
	}
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
		b.await = awaiting{}
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
	case "acc":
		b.await = awaiting{}
		return b.accessScreen(arg), ""
	case "ax":
		name, how, _ := strings.Cut(arg, ":")
		return b.setExpiry(name, how), ""
	case "axin":
		b.await = awaiting{kind: "expire", name: arg, at: time.Now()}
		return screen{b.tr("ui.expire.prompt", html.EscapeString(arg)), keyboard{{b.btn("b.cancel", "acc:"+arg)}}}, ""
	case "tg":
		return b.telegramScreen(arg), ""
	case "tgme":
		return b.link(arg, true), ""
	case "tgoff":
		return b.link(arg, false), ""
	case "ren":
		b.await = awaiting{kind: "rename", name: arg, at: time.Now()}
		return screen{b.tr("ui.rename.prompt", html.EscapeString(arg)), keyboard{{b.btn("b.cancel", "c:"+arg)}}}, ""
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
	case "mute":
		return b.toggleMute(arg), ""
	case "lim":
		return b.setLimit(arg), ""
	case "gw":
		return b.gatewayScreen(), ""
	case "srv":
		return b.serverScreen(), ""
	case "rsn":
		err := b.change(func() error { return restartNode(b.s, arg, io.Discard) })
		if err != nil {
			return b.failed(err, b.btn("b.back", "c:"+arg)), ""
		}
		return b.clientScreen(arg), b.tr("ui.node.restarting")
	case "gws":
		return b.chooseWorld(arg)
	case "gwr":
		return b.russiaAsk(arg), ""
	case "gwr!":
		mode, at, _ := strings.Cut(arg, ":")
		if !fresh(at) {
			return b.gatewayScreen(), b.tr("ui.expired.button")
		}
		return b.setRussia(mode), ""
	case "gwc":
		return b.carrierAsk(arg), ""
	case "gwc!":
		mode, at, _ := strings.Cut(arg, ":")
		if !fresh(at) {
			return b.gatewayScreen(), b.tr("ui.expired.button")
		}
		return b.setCarrier(mode), ""
	case "rqs":
		return b.requestsScreen(), ""
	case "rq", "rq1":
		return b.requestPress(action, arg), ""
	case "dc", "dp", "du", "dr", "dr!":
		return b.docsPress(action, arg), ""
	case "rnk":
		c, err := b.s.Get(arg)
		if err != nil || c.Telegram == nil || nick(*c.Telegram) == "" {
			return b.telegramScreen(arg), ""
		}
		return b.rename(arg, nick(*c.Telegram)), ""
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
	seen    time.Time // last connected (seen.go); zero when unknown
}

type statusView struct {
	online    bool
	up        time.Duration
	down, upB uint64
	doc       int // the document the traffic goes over, -1 unknown (docs.go)
}

func (b *bot) clientViews(clients []Client) []clientView { return viewClients(b.s, clients) }

// viewClients gathers what the screens (bot and web) show about clients:
// their node's container and, from the node, the client's status.
func viewClients(s Store, clients []Client) []clientView {
	now := time.Now()
	states := containerStates()
	live := nodeStatuses(s, activeClients(clients, now))
	seen := s.lastSeen()
	var out []clientView
	for _, c := range clients {
		v := clientView{c: c, active: c.Active(now), running: states["reflux-node-"+c.Name] != "", seen: seen[c.Name]}
		if st, ok := live[c.Name]; ok {
			v.status = &statusView{online: st.Connected, up: time.Duration(st.UptimeMs) * time.Millisecond,
				down: st.BytesOut, upB: st.BytesIn, doc: c.docIndex(st.Active)}
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

// onlineText says whether the client is connected, and when it last was.
func (v clientView) onlineText(l lang) string {
	switch {
	case v.status != nil && v.status.online:
		return tr(l, "online")
	case !v.seen.IsZero():
		return tr(l, "offline.seen", durationIn(l, time.Since(v.seen)))
	}
	return tr(l, "offline")
}

// state is a client's state in words.
func (b *bot) state(v clientView) string { return stateText(b.lang, v) }

// stateText is a client's state in words in l: its access when it is off,
// else its node and whether the client is online, with the traffic.
func stateText(l lang, v clientView) string {
	switch {
	case !v.active:
		p := accessPhrase(v.c, time.Now())
		return tr(l, p.id, p.args...)
	case !v.running:
		return tr(l, "ui.node.down")
	case v.status == nil:
		return tr(l, "ui.nostatus")
	}
	return v.onlineText(l) + " · ↓" + humanBytes(v.status.down) + " ↑" + humanBytes(v.status.upB)
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
		fmt.Fprintf(&t, "%s <b>%s</b>%s · %s\n", v.mark(), html.EscapeString(v.c.Name), owner(v.c), b.state(v))
	}
	t.WriteString("\n" + b.tr("ui.updated", time.Now().Format("15:04:05")))
	kb := keyboard{
		{b.btn("b.refresh", "home"), b.btn("b.doctor", "doc")},
		{b.btn("b.clients", "cls"), b.btn("b.add", "add")},
		{b.btn("b.server", "srv"), b.btn("b.gateway", "gw")},
		{b.btn("b.speed", "sp"), b.btn("b.settings", "set")},
	}
	if n := len(b.s.pendingRequests()); n > 0 {
		kb = append(keyboard{{b.btn("b.rq", "rqs", n)}}, kb...)
	}
	return screen{t.String(), kb}
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
	if c.Note != "" {
		t.WriteString(b.tr("ui.client.note", html.EscapeString(c.Note)) + "\n")
	}
	t.WriteString(b.tr("ui.client.access", b.tr(p.id, p.args...)) + "\n")
	switch {
	case !v.active:
		t.WriteString(b.tr("ui.client.node", b.tr("ui.node.stopped")) + "\n")
	case !v.running:
		t.WriteString(b.tr("ui.client.node", b.tr("ui.node.notrunning")) + "\n")
	case v.status == nil:
		t.WriteString(b.tr("ui.client.node", b.tr("ui.nostatus")) + "\n")
	default:
		t.WriteString(b.tr("ui.client.node", b.tr("ui.node.up", durationIn(b.lang, v.status.up))) + "\n")
		t.WriteString(b.tr("ui.client.client", v.onlineText(b.lang)) + "\n")
		t.WriteString(b.tr("ui.client.traffic", humanBytes(v.status.down), humanBytes(v.status.upB)) + "\n")
		if v.status.online && v.status.doc > 0 {
			t.WriteString(b.tr("ui.client.onbackup", v.status.doc+1) + "\n")
		}
	}
	if today, month, ok := b.s.trafficNow(c.Name, now); ok {
		t.WriteString(b.tr("ui.client.days", humanBytes(today.Down), humanBytes(today.Up), humanBytes(month.Down), humanBytes(month.Up)) + "\n")
	}
	if c.session() {
		t.WriteString(b.tr("ui.client.docs", len(c.Docs())) + "\n")
	}
	tg := b.tr("ui.tg.none")
	if c.Telegram != nil {
		tg = html.EscapeString(c.Telegram.String())
	}
	t.WriteString(b.tr("ui.client.telegram", tg) + "\n")
	t.WriteString(b.tr("ui.client.created", c.Created.Local().Format(time.DateOnly)))
	toggle := b.btn("b.pause", "pa:"+c.Name)
	if c.Paused {
		toggle = b.btn("b.resume", "re:"+c.Name)
	}
	return screen{t.String(), keyboard{
		{b.btn("b.qr", "qr:"+c.Name), toggle},
		{b.btn("b.access", "acc:"+c.Name), b.btn("b.telegram", "tg:"+c.Name)},
		{b.btn("b.docs", "dc:"+c.Name, len(c.Docs()))},
		{b.btn("b.rename", "ren:"+c.Name), b.btn("b.logs", "lg:"+c.Name)},
		{b.btn("b.restartnode", "rsn:"+c.Name), b.btn("b.revoke", "rv:"+c.Name)},
		{b.btn("b.clients", "cls"), b.btn("b.home", "home")},
	}}
}

func (b *bot) settingsScreen() screen {
	var t strings.Builder
	t.WriteString(b.tr("ui.settings", b.tr("lang.name")) + "\n\n" + b.tr("ui.alerts") + "\n")
	kb := keyboard{{{Text: "🇷🇺 Русский", Data: "lang:ru"}, {Text: "🇬🇧 English", Data: "lang:en"}}}
	for _, c := range alertCategories {
		mark := "🔔"
		if b.mute[c] {
			mark = "🔕"
		}
		fmt.Fprintf(&t, "%s %s\n", mark, b.tr("alerts."+c+".about"))
		kb = append(kb, []tgButton{{Text: mark + " " + b.tr("alerts."+c), Data: "mute:" + c}})
	}
	lim := b.s.hostLimits()
	temp := b.tr("ui.limit.sensor")
	if lim.TempWarn > 0 {
		temp = b.tr("ui.limit.deg", lim.TempWarn)
	} else if ts := readTemps(); len(ts) > 0 {
		temp = b.tr("ui.limit.deg.sensor", int(lim.tempWarnAt(ts[0])))
	}
	t.WriteString("\n" + b.tr("ui.limits", temp, lim.CPUWarn) + "\n")
	kb = append(kb,
		[]tgButton{b.btn("b.temp.down", "lim:t-"), b.btn("b.temp.up", "lim:t+")},
		[]tgButton{b.btn("b.cpu.down", "lim:c-"), b.btn("b.cpu.up", "lim:c+")},
		[]tgButton{b.btn("b.home", "home")})
	return screen{t.String(), kb}
}

// setLimit moves a server warning's threshold a step: the temperature to
// the next multiple of 5 °C (60-100), the CPU by 10% (50-100).
func (b *bot) setLimit(arg string) screen {
	c, err := b.s.loadBotConfig()
	if err != nil {
		return b.failed(err, b.btn("b.home", "home"))
	}
	lim := b.s.hostLimits()
	switch arg {
	case "t-", "t+":
		at := lim.TempWarn
		if at == 0 {
			at = defaultTemp
			if ts := readTemps(); len(ts) > 0 {
				at = int(lim.tempWarnAt(ts[0]))
			}
		}
		// To the next multiple of 5 down or up: 82 goes to 80 or 85.
		if arg == "t-" {
			at = (at - 1) / 5 * 5
		} else {
			at = at/5*5 + 5
		}
		c.TempWarn = min(max(at, 60), 100)
	case "c-", "c+":
		at := lim.CPUWarn
		if arg == "c-" {
			at -= 10
		} else {
			at += 10
		}
		c.CPUWarn = min(max(at, 50), 100)
	}
	if err := b.s.saveBotConfig(c); err != nil {
		log.Printf("bot: saving the settings: %v", err)
	}
	return b.settingsScreen()
}

// toggleMute switches an alert category off or back on and saves it.
func (b *bot) toggleMute(cat string) screen {
	known := false
	for _, c := range alertCategories {
		known = known || c == cat
	}
	if known {
		b.mute[cat] = !b.mute[cat]
		b.saveSettings()
	}
	return b.settingsScreen()
}

// saveSettings writes the language and the muted categories to
// telegram.json.
func (b *bot) saveSettings() {
	c, err := b.s.loadBotConfig()
	if err == nil {
		c.Lang, c.Mute = string(b.lang), nil
		for _, cat := range alertCategories {
			if b.mute[cat] {
				c.Mute = append(c.Mute, cat)
			}
		}
		err = b.s.saveBotConfig(c)
	}
	if err != nil {
		log.Printf("bot: saving the settings: %v", err)
	}
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

// expireTo sets the expiry from what the owner typed: never, a date, 30d.
func (b *bot) expireTo(name, when string) screen {
	t, err := parseExpiry(when, time.Now())
	if err == nil {
		err = b.change(func() error {
			return setAccess(b.s, name, io.Discard, func(c *Client) error { c.Expires = t; return nil })
		})
	}
	if err != nil {
		return b.failed(err, b.btn("b.back", "acc:"+name))
	}
	return b.accessScreen(name)
}

// accessDays are the extensions the access screen offers.
var accessDays = []int{1, 7, 30, 90, 365}

func (b *bot) accessScreen(name string) screen {
	c, err := b.s.Get(name)
	if err != nil {
		return b.failed(err, b.btn("b.clients", "cls"))
	}
	now := time.Now()
	p := accessPhrase(c, now)
	var t strings.Builder
	fmt.Fprintf(&t, "%s\n%s", b.tr("ui.access.title", html.EscapeString(c.Name)), b.tr("ui.client.access", b.tr(p.id, p.args...)))
	if !c.Expires.IsZero() && now.Before(c.Expires) {
		t.WriteString(" · " + b.tr("ui.access.left", durationIn(b.lang, c.Expires.Sub(now))))
	}
	t.WriteString("\n\n" + b.tr("ui.access.hint"))
	var plus []tgButton
	for _, d := range accessDays {
		plus = append(plus, tgButton{Text: b.tr(fmt.Sprintf("b.plus%d", d)), Data: fmt.Sprintf("ax:%s:+%d", c.Name, d)})
	}
	return screen{t.String(), keyboard{
		plus[:3], plus[3:],
		{b.btn("b.never", "ax:"+c.Name+":never"), b.btn("b.endnow", "ax:"+c.Name+":now")},
		{b.btn("b.typedate", "axin:"+c.Name)},
		{b.btn("b.back", "c:"+c.Name), b.btn("b.home", "home")},
	}}
}

// setExpiry applies an access screen button: +N days from the end of the
// access while it lasts (from now once it ended, or for unlimited
// access), no limit, or an end right now.
func (b *bot) setExpiry(name, how string) screen {
	err := b.change(func() error {
		return setAccess(b.s, name, io.Discard, func(c *Client) error {
			now := time.Now().Truncate(time.Second)
			switch {
			case how == "never":
				c.Expires = time.Time{}
			case how == "now":
				c.Expires = now
			case strings.HasPrefix(how, "+"):
				days, err := strconv.Atoi(how[1:])
				if err != nil || days <= 0 || days > 3650 {
					return fmt.Errorf("bad extension %q", how)
				}
				from := now
				if c.Expires.After(now) {
					from = c.Expires
				}
				c.Expires = from.AddDate(0, 0, days)
			default:
				return fmt.Errorf("bad expiry %q", how)
			}
			return nil
		})
	})
	if err != nil {
		return b.failed(err, b.btn("b.back", "acc:"+name))
	}
	return b.accessScreen(name)
}

func (b *bot) telegramScreen(name string) screen {
	c, err := b.s.Get(name)
	if err != nil {
		return b.failed(err, b.btn("b.clients", "cls"))
	}
	now := b.tr("ui.tg.none")
	if c.Telegram != nil {
		now = html.EscapeString(c.Telegram.String())
	}
	kb := keyboard{{b.btn("b.tg.me", "tgme:"+c.Name)}}
	if c.Telegram != nil {
		if n := nick(*c.Telegram); n != "" && n != c.Name {
			kb = append(kb, []tgButton{b.btn("b.tg.rename", "rnk:"+c.Name, n)})
		}
		kb = append(kb, []tgButton{b.btn("b.tg.off", "tgoff:"+c.Name)})
	}
	kb = append(kb, []tgButton{b.btn("b.back", "c:"+c.Name), b.btn("b.home", "home")})
	return screen{b.tr("ui.tg.title", html.EscapeString(c.Name)) + "\n" + b.tr("ui.client.telegram", now) + "\n\n" + b.tr("ui.tg.hint"), kb}
}

// link binds a client to the owner's own account (the one pressing the
// button), or unbinds it. Others will come with the client bot.
func (b *bot) link(name string, me bool) screen {
	from := b.linkTo
	b.linkTo = nil
	err := b.change(func() error {
		c, err := b.s.Get(name)
		if err != nil {
			return err
		}
		c.Telegram = nil
		if me {
			if from == nil {
				return errors.New("no account to link")
			}
			c.Telegram = &TGAccount{ID: from.ID, Username: from.Username, Name: from.FirstName}
		}
		return b.s.Save(c)
	})
	if err != nil {
		return b.failed(err, b.btn("b.back", "tg:"+name))
	}
	// The Telegram screen: it offers the account's nickname as the name.
	return b.telegramScreen(name)
}

// nick is a Telegram username as a client name (lowercase, dashes for
// underscores), "" when there is none or it does not fit.
func nick(a TGAccount) string {
	n := strings.Trim(strings.ReplaceAll(strings.ToLower(a.Username), "_", "-"), "-")
	if validName(n) != nil {
		return ""
	}
	return n
}

func (b *bot) rename(name, to string) screen {
	err := b.change(func() error { return cmdRename(b.s, name, to, io.Discard) })
	if err != nil {
		return b.failed(err, b.btn("b.back", "c:"+name))
	}
	sc := b.clientScreen(to)
	sc.text = b.tr("ui.rename.done", html.EscapeString(name), html.EscapeString(to)) + "\n\n" + sc.text
	return sc
}

// speed runs the speed test through the egress.
func (b *bot) speed() screen {
	var t strings.Builder
	t.WriteString(b.tr("ui.speed.title") + "\n")
	for _, l := range speedLines(b.lang, speedTest()) {
		t.WriteString(html.EscapeString(l) + "\n")
	}
	t.WriteString("\n" + b.tr("ui.speed.hint") + "\n" + b.tr("ui.updated", time.Now().Format("15:04:05")))
	return screen{t.String(), keyboard{{b.btn("b.speed", "sp"), b.btn("b.home", "home")}}}
}

// owner is the Telegram account after a client's name, if linked.
func owner(c Client) string {
	if c.Telegram == nil {
		return ""
	}
	name := c.Telegram.Name
	if name == "" {
		name = c.Telegram.String()
	}
	return " (" + html.EscapeString(name) + ")"
}

func (b *bot) addPrompt() screen {
	b.await = awaiting{kind: "add", at: time.Now()}
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
	b.saveSettings()
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
	msg := b.tr("ui.show.text", e(c.Name), mins, b.tr(p.id, p.args...), e(c.Transport), e(c.URL), e(key), e(link))
	if c.session() {
		var docs strings.Builder
		for i, d := range c.Docs() {
			docs.WriteString(b.tr("ui.show.doc", i+1, e(d.Transport), docPriority(i), e(d.URL)) + "\n")
		}
		msg = b.tr("ui.show.session", e(c.Name), mins, b.tr(p.id, p.args...), docs.String(), e(c.context()), e(key), e(link))
	}
	text, err := b.t.send(b.chat, msg)
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
