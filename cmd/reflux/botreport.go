package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/host"
)

// The weekly report: from Monday 10:00 the bot sends what last week was —
// each client's traffic and busiest day, the alerts, whose access ends
// soon, the disk — once a week, unless the owner switched the "report"
// group off. /report sends the last 7 days any time.

const (
	reportHour  = 10
	reportAhead = 14 * 24 * time.Hour // access ending this soon is listed
)

func (s Store) reportStatePath() string { return filepath.Join(s.Root, "report.json") }

type reportState struct {
	Week string `json:"week"` // the ISO week the last report went out in
}

// isoWeek names the week t is in: "2026-W40".
func isoWeek(t time.Time) string {
	y, w := t.ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// reportDue: a report for last week has not gone out this week, and it is
// past Monday reportHour. The first time, the report starts next week.
func (s Store) reportDue(now time.Time) bool {
	var st reportState
	b, err := os.ReadFile(s.reportStatePath())
	if errors.Is(err, fs.ErrNotExist) {
		s.reportSent(now)
		return false
	}
	if err == nil {
		json.Unmarshal(b, &st)
	}
	if now.Weekday() == time.Monday && now.Hour() < reportHour {
		return false
	}
	return st.Week != isoWeek(now)
}

func (s Store) reportSent(now time.Time) error {
	return writeJSON(s.reportStatePath(), reportState{Week: isoWeek(now)})
}

// lastWeek is Monday to Monday before the week now is in, local time.
func lastWeek(now time.Time) (from, to time.Time) {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	to = day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7)) // this Monday
	return to.AddDate(0, 0, -7), to
}

// clientWeek is a client's traffic over the report's days.
type clientWeek struct {
	Down, Up uint64
	Top      string // the busiest day, 02.01
	TopBytes uint64
	Counted  bool // the panel keeps its traffic
}

func (s Store) clientWeek(name string, from, to time.Time) clientWeek {
	var w clientWeek
	b, err := os.ReadFile(s.trafficPath(name))
	if err != nil {
		return w
	}
	var tf trafficFile
	if json.Unmarshal(b, &tf) != nil {
		return w
	}
	w.Counted = true
	days := make([]string, 0, len(tf.Days))
	for d := range tf.Days {
		days = append(days, d)
	}
	sort.Strings(days)
	for _, d := range days {
		at, err := time.ParseInLocation(time.DateOnly, d, from.Location())
		if err != nil || at.Before(from) || !at.Before(to) {
			continue
		}
		t := tf.Days[d]
		w.Down, w.Up = w.Down+t.Down, w.Up+t.Up
		if t.Down+t.Up > w.TopBytes {
			w.Top, w.TopBytes = at.Format("02.01"), t.Down+t.Up
		}
	}
	return w
}

// trafficNow is a client's traffic today and this month, from the
// panel's traffic.json; ok is false when the panel keeps none.
func (s Store) trafficNow(name string, now time.Time) (today, month dayTraffic, ok bool) {
	b, err := os.ReadFile(s.trafficPath(name))
	if err != nil {
		return today, month, false
	}
	var tf trafficFile
	if json.Unmarshal(b, &tf) != nil {
		return today, month, false
	}
	day, mon := now.Format(time.DateOnly), now.Format("2006-01")
	for d, t := range tf.Days {
		if strings.HasPrefix(d, mon) {
			month.Down, month.Up = month.Down+t.Down, month.Up+t.Up
		}
		if d == day {
			today = t
		}
	}
	return today, month, true
}

// idleAfter is how long without traffic makes a channel worth a line.
const idleAfter = 14 * 24 * time.Hour

// lastUsed is the last day a client's channel carried traffic (zero:
// never); counted is false when the panel keeps no traffic for it.
func (s Store) lastUsed(name string) (last time.Time, counted bool) {
	b, err := os.ReadFile(s.trafficPath(name))
	if err != nil {
		return last, false
	}
	var tf trafficFile
	if json.Unmarshal(b, &tf) != nil {
		return last, false
	}
	for d, t := range tf.Days {
		// A megabyte a day: keepalives alone are not use.
		if t.Down+t.Up < 1e6 {
			continue
		}
		if at, err := time.ParseInLocation(time.DateOnly, d, time.Local); err == nil && at.After(last) {
			last = at
		}
	}
	return last, true
}

// weeklyReport is the report on [from, to); title names the span.
func weeklyReport(s Store, l lang, title string, from, to, now time.Time) string {
	t := func(id string, a ...any) string { return tr(l, id, a...) }
	e := html.EscapeString
	var b strings.Builder
	b.WriteString(title + "\n\n" + t("rep.clients") + "\n")
	clients, _ := s.List()
	counted := false
	var down, up uint64
	for _, c := range clients {
		w := s.clientWeek(c.Name, from, to)
		counted = counted || w.Counted
		down, up = down+w.Down, up+w.Up
		state := ""
		if !c.Active(now) {
			p := accessPhrase(c, now)
			state = " (" + t(p.ID, p.Args...) + ")"
		}
		switch {
		case w.Down+w.Up == 0:
			fmt.Fprintf(&b, "• <b>%s</b>%s: %s\n", e(c.Name), state, t("rep.idle"))
		default:
			fmt.Fprintf(&b, "• <b>%s</b>%s: ↓ %s ↑ %s, %s\n", e(c.Name), state, humanBytes(w.Down), humanBytes(w.Up),
				t("rep.top", w.Top, humanBytes(w.TopBytes)))
		}
	}
	switch {
	case len(clients) == 0:
		b.WriteString(t("ui.clients.none") + "\n")
	case !counted:
		b.WriteString(t("rep.nocount") + "\n")
	case len(clients) > 1:
		b.WriteString(t("rep.total", humanBytes(down), humanBytes(up)) + "\n")
	}

	// Alerts in the span.
	var fails, warns, oks int
	for _, ev := range s.readEvents(eventsMax) {
		if ev.At.Before(from) || !ev.At.Before(to) {
			continue
		}
		switch ev.Level {
		case "FAIL":
			fails++
		case "warn":
			warns++
		case "ok":
			oks++
		}
	}
	if fails+warns+oks == 0 {
		b.WriteString("\n" + t("rep.events.none") + "\n")
	} else {
		b.WriteString("\n" + t("rep.events", fails, warns, oks) + "\n")
	}

	// Channels not used for a while: a client who left, or one whose
	// channel stopped working for them; either way the owner should know.
	var idle []string
	for _, c := range clients {
		if !c.Active(now) || now.Sub(c.Created) < idleAfter {
			continue
		}
		last, counted := s.lastUsed(c.Name)
		switch {
		case !counted:
		case last.IsZero():
			idle = append(idle, t("rep.unused.never", e(c.Name)))
		case now.Sub(last) >= idleAfter:
			idle = append(idle, t("rep.unused.one", e(c.Name), last.Format("02.01"), durationIn(l, now.Sub(last))))
		}
	}
	if len(idle) > 0 {
		b.WriteString("\n" + t("rep.unused") + "\n" + strings.Join(idle, "\n") + "\n")
	}

	// Access ending soon.
	var ending []string
	for _, c := range clients {
		if c.Active(now) && !c.Expires.IsZero() && c.Expires.Sub(now) < reportAhead {
			ending = append(ending, t("rep.ends.one", e(c.Name), c.Expires.Local().Format("02.01 15:04"), durationIn(l, c.Expires.Sub(now))))
		}
	}
	if len(ending) > 0 {
		b.WriteString("\n" + t("rep.ends") + "\n" + strings.Join(ending, "\n") + "\n")
	}

	// The tunnels' availability.
	if world, russia, _ := s.uptimeSpan(from, to); world[1] > 0 {
		b.WriteString("\n" + t("rep.uptime.tunnels", world.percent(), russia.percent()) + "\n")
	}

	// The server.
	var server []string
	for _, d := range readDisks() {
		if d.Mount == "/" {
			server = append(server, t("rep.disk", d.Percent()))
		}
	}
	if up, err := host.ReadUptime(); err == nil {
		server = append(server, t("rep.uptime", durationIn(l, up)))
	}
	if len(server) > 0 {
		b.WriteString("\n" + t("rep.server", strings.Join(server, ", ")) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// sendReport sends last week's report when it is due. The caller holds
// b.mu.
func (b *bot) sendReport(now time.Time) {
	if b.mute["report"] || !b.s.reportDue(now) {
		return
	}
	from, to := lastWeek(now)
	title := b.tr("rep.title", from.Format("02.01"), to.AddDate(0, 0, -1).Format("02.01"))
	if _, err := b.t.Send(b.chat, weeklyReport(b.s, b.lang, title, from, to, now)); err != nil {
		return // the next run tries again
	}
	b.s.reportSent(now)
}

// reportScreen is the last 7 days, for /report.
func (b *bot) reportScreen() screen {
	now := time.Now()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	from, to := day.AddDate(0, 0, -6), day.AddDate(0, 0, 1)
	return screen{weeklyReport(b.s, b.lang, b.tr("rep.title7"), from, to, now), keyboard{{b.btn("b.home", "home")}}}
}
