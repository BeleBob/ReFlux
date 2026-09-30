package main

import (
	"fmt"
	"html"
	"io"
	"slices"
	"strings"
	"time"
)

// Screens that summarize many things: checks, clients, gateway. They show
// what works in a short line each and what does not in full, with its
// hint.

// sections are the groups of the checks screen, in order, with the alert
// categories they match.
var sections = []struct{ cat, icon string }{
	{"tunnels", "🌐"},
	{"nodes", "👥"},
	{"updates", "📦"},
	{"server", "🖥"},
}

func (b *bot) doctorScreen() screen {
	fs := runChecks(b.s)
	warns, fails := count(fs)
	var t strings.Builder
	fmt.Fprintf(&t, "%s · %s\n", b.tr("ui.doctor.title"), time.Now().Format("15:04"))
	if warns+fails == 0 {
		t.WriteString(b.tr("ui.doctor.allok") + "\n")
	} else {
		t.WriteString(b.tr("ui.doctor.sum", fails, warns) + "\n")
	}
	for _, sec := range sections {
		var lines []string
		for _, f := range fs {
			if category(f.Key) != sec.cat {
				continue
			}
			if f.Level == levelOK {
				lines = append(lines, "✅ "+html.EscapeString(b.short(f)))
			} else {
				lines = append(lines, alertLine(f, b.lang))
			}
		}
		if len(lines) > 0 {
			fmt.Fprintf(&t, "\n%s <b>%s</b>\n%s\n", sec.icon, b.tr("alerts."+sec.cat), strings.Join(lines, "\n"))
		}
	}
	return screen{t.String(), keyboard{
		{b.btn("b.refresh", "doc"), b.btn("b.restart", "rs")},
		{b.btn("b.home", "home")},
	}}
}

// short is a working check in a few words.
func (b *bot) short(f finding) string { return shortText(b.s, b.lang, f) }

// shortText is a working check in a few words, in l.
func shortText(s Store, l lang, f finding) string {
	t := func(id string, args ...any) string { return tr(l, id, args...) }
	a := f.Args
	str := func(i int) string {
		if i >= len(a) {
			return ""
		}
		if p, ok := a[i].(phrase); ok {
			return t(p.id, p.args...)
		}
		return fmt.Sprint(a[i])
	}
	switch f.Msg {
	case "module.ok":
		return t("short.module")
	case "hostrule.ok":
		return t("short.hostrule")
	case "configs.ok":
		return t("short.configs", len(strings.Split(str(0), ",")), t("gw.ru."+s.russiaMode()))
	case "docker.ok":
		return t("short.docker", str(0), str(1))
	case "image.ok":
		return t("short.image", imageName(str(0)), str(1), str(2))
	case "image.local":
		return t("short.image.local", imageName(str(0)))
	case "egress.ok":
		if strings.Contains(str(0), "healthy") && !strings.Contains(str(0), "unhealthy") {
			return t("short.egress.ok")
		}
		return t("short.egress", str(0))
	case "world.up":
		return t("short.world", strings.TrimSuffix(str(0), ".conf"), shortTime(str(1)))
	case "russia.up":
		return t("short.russia", str(0), a[1])
	case "killswitch":
		return t("short.killswitch", a[0])
	case "carrier.direct":
		return t("short.carrier", a[0])
	case "node.ok":
		return t("short.node", str(0), str(1), str(2), str(3), str(4))
	case "node.inactive":
		return str(0) + ": " + str(1)
	case "cron.ok":
		return t("short.cron")
	case "disk.ok":
		return t("short.disk", str(0), a[1])
	}
	return f.text(l)
}

// shortTime shortens a local "2006-01-02 15:04:05": the time of day for
// today, the day and time before.
func shortTime(s string) string {
	t, err := time.ParseInLocation(time.DateTime, s, time.Local)
	if err != nil {
		return s
	}
	if t.Format(time.DateOnly) == time.Now().Format(time.DateOnly) {
		return t.Format("15:04")
	}
	return t.Format("02.01 15:04")
}

// imageName shortens ghcr.io/belebob/reflux-node:main to node.
func imageName(ref string) string {
	name := ref[strings.LastIndex(ref, "/")+1:]
	name, _, _ = strings.Cut(name, ":")
	return strings.TrimPrefix(name, "reflux-")
}

func (b *bot) clientsScreen() screen {
	clients, err := b.s.List()
	if err != nil {
		return b.failed(err, b.btn("b.home", "home"))
	}
	var t strings.Builder
	fmt.Fprintf(&t, "%s · %d\n", b.tr("ui.clients"), len(clients))
	if len(clients) == 0 {
		t.WriteString("\n" + b.tr("ui.clients.none") + "\n")
	}
	kb := keyboard{}
	var row []tgButton
	now := time.Now()
	for _, v := range b.clientViews(clients) {
		fmt.Fprintf(&t, "\n%s <b>%s</b>%s\n", v.mark(), html.EscapeString(v.c.Name), owner(v.c))
		detail := b.state(v)
		if v.active {
			p := accessPhrase(v.c, now)
			detail += " · " + b.tr(p.id, p.args...)
		}
		t.WriteString("      " + detail + "\n")
		row = append(row, tgButton{Text: v.mark() + " " + v.c.Name, Data: "c:" + v.c.Name})
		if len(row) == 2 {
			kb, row = append(kb, row), nil
		}
	}
	if row != nil {
		kb = append(kb, row)
	}
	if len(clients) > 0 {
		t.WriteString("\n<i>" + b.tr("ui.clients.hint") + "</i>")
	}
	kb = append(kb, []tgButton{b.btn("b.add", "add"), b.btn("b.home", "home")})
	return screen{t.String(), kb}
}

// ---- gateway ----

func (b *bot) gatewayScreen() screen {
	var t strings.Builder
	t.WriteString(b.tr("ui.gw.title") + "\n\n")
	st, stErr := readEgressStatus()
	chosen := b.s.chosenWorld()
	how := b.tr("ui.gw.auto")
	if chosen != "" {
		how = html.EscapeString(strings.TrimSuffix(chosen, ".conf"))
	}
	if stErr == nil {
		mark := "✅"
		if !st.WorldOK {
			mark = "❌"
		}
		t.WriteString(b.tr("ui.gw.world", mark+" "+html.EscapeString(st.World)) + "\n")
		russia := "✅ " + b.tr(ruMode(st).id)
		if !st.RUOK {
			russia = "❌ " + b.tr(ruMode(st).id)
		}
		t.WriteString(b.tr("ui.russia", russia) + "\n")
	} else {
		t.WriteString(b.tr("ui.egress.none") + "\n")
	}
	carrier := b.tr("gw.carrier.tunnel")
	if b.s.carrierDirect() {
		carrier = b.tr("gw.carrier.direct")
		if stErr == nil && st.CarrierDirect {
			carrier += " · " + b.tr("gw.carrier.addrs", len(st.Carriers))
		}
	}
	t.WriteString(b.tr("ui.gw.choice", how) + "\n" + b.tr("ui.gw.rumode", b.tr("gw.ru."+b.s.russiaMode())) + "\n" +
		b.tr("ui.gw.carrier", carrier) + "\n\n" + b.tr("ui.gw.hint"))

	kb := keyboard{}
	var row []tgButton
	for _, w := range b.s.worldConfigs() {
		label := strings.TrimSuffix(w, ".conf")
		if stErr == nil && w == st.World {
			label = "✅ " + label
		} else if w == chosen {
			label = "⏳ " + label
		}
		row = append(row, tgButton{Text: label, Data: "gws:" + w})
		if len(row) == 3 {
			kb, row = append(kb, row), nil
		}
	}
	if row != nil {
		kb = append(kb, row)
	}
	kb = append(kb, []tgButton{b.btn("b.auto", "gws:auto")})
	var ru []tgButton
	for _, m := range russiaModes {
		label := b.tr("b.ru." + m)
		if m == b.s.russiaMode() {
			label = "✅ " + label
		}
		ru = append(ru, tgButton{Text: label, Data: "gwr:" + m})
	}
	var carrierRow []tgButton
	for _, m := range []string{"direct", "tunnel"} {
		label := b.tr("b.carrier." + m)
		if (m == "direct") == b.s.carrierDirect() {
			label = "✅ " + label
		}
		carrierRow = append(carrierRow, tgButton{Text: label, Data: "gwc:" + m})
	}
	kb = append(kb, ru, carrierRow, []tgButton{b.btn("b.refresh", "gw"), b.btn("b.home", "home")})
	return screen{t.String(), kb}
}

// chooseWorld asks the egress for another world server; it switches
// within a probe round.
func (b *bot) chooseWorld(name string) (screen, string) {
	err := b.change(func() error { return b.s.chooseWorld(name) })
	if err != nil {
		return b.failed(err, b.btn("b.gateway", "gw")), ""
	}
	toast := b.tr("ui.gw.auto.set")
	if name != "auto" {
		toast = b.tr("ui.gw.switching", strings.TrimSuffix(name, ".conf"))
	}
	return b.gatewayScreen(), toast
}

func (b *bot) russiaAsk(mode string) screen {
	if !slices.Contains(russiaModes, mode) {
		return b.gatewayScreen()
	}
	return screen{b.tr("ui.gw.rusure", b.tr("gw.ru."+mode)), keyboard{
		{b.btn("b.confirm.ru", "gwr!:"+mode+":"+stamp())},
		{b.btn("b.cancel", "gw")},
	}}
}

func (b *bot) carrierAsk(mode string) screen {
	if mode != "direct" && mode != "tunnel" {
		return b.gatewayScreen()
	}
	return screen{b.tr("ui.gw.carriersure", b.tr("gw.carrier."+mode)), keyboard{
		{b.btn("b.confirm.ru", "gwc!:"+mode+":"+stamp())},
		{b.btn("b.cancel", "gw")},
	}}
}

// setCarrier routes the mail.ru channel directly or through the Russian
// tunnel; the egress reads it at start, so it restarts with the nodes.
func (b *bot) setCarrier(mode string) screen {
	err := b.change(func() error {
		if err := b.s.setCarrierDirect(mode == "direct"); err != nil {
			return err
		}
		return cmdRestart(b.s, io.Discard)
	})
	if err != nil {
		return b.failed(err, b.btn("b.gateway", "gw"))
	}
	sc := b.gatewayScreen()
	sc.text = b.tr("ui.restart.done") + "\n\n" + sc.text
	return sc
}

// setRussia changes how Russia leaves and restarts the egress (and the
// nodes in its namespace), which reads it at start.
func (b *bot) setRussia(mode string) screen {
	err := b.change(func() error {
		if err := b.s.setRussiaMode(mode); err != nil {
			return err
		}
		return cmdRestart(b.s, io.Discard)
	})
	if err != nil {
		return b.failed(err, b.btn("b.gateway", "gw"))
	}
	sc := b.gatewayScreen()
	sc.text = b.tr("ui.restart.done") + "\n\n" + sc.text
	return sc
}
