package main

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLastWeek(t *testing.T) {
	for now, want := range map[string]string{
		"2026-10-07 15:00": "2026-09-28 2026-10-05", // a Wednesday
		"2026-10-05 09:00": "2026-09-28 2026-10-05", // Monday morning
		"2026-10-04 23:00": "2026-09-21 2026-09-28", // Sunday night
	} {
		at, _ := time.ParseInLocation("2006-01-02 15:04", now, time.Local)
		from, to := lastWeek(at)
		if got := from.Format(time.DateOnly) + " " + to.Format(time.DateOnly); got != want {
			t.Errorf("lastWeek(%s) = %s, want %s", now, got, want)
		}
	}
}

func TestReportDue(t *testing.T) {
	s := Store{Root: t.TempDir()}
	at := func(v string) time.Time {
		t, _ := time.ParseInLocation("2006-01-02 15:04", v, time.Local)
		return t
	}
	if s.reportDue(at("2026-10-07 12:00")) {
		t.Error("due at once on the first run")
	}
	for v, want := range map[string]bool{
		"2026-10-11 20:00": false, // the same week
		"2026-10-12 09:59": false, // Monday before 10
		"2026-10-12 10:00": true,
		"2026-10-14 08:00": true, // the bot was down on Monday
	} {
		if got := s.reportDue(at(v)); got != want {
			t.Errorf("due at %s = %v", v, got)
		}
	}
	s.reportSent(at("2026-10-12 10:00"))
	if s.reportDue(at("2026-10-12 10:01")) || s.reportDue(at("2026-10-18 23:00")) {
		t.Error("due again in the week it went out")
	}
	if !s.reportDue(at("2026-10-19 10:00")) {
		t.Error("not due the next Monday")
	}
}

func TestWeeklyReport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	proc, _ := fakeHostTree(t)
	writeFile(t, proc+"/uptime", "1209600 1\n")
	oldCmd := runCmd
	runCmd = func(stdout io.Writer, name string, args ...string) error {
		if name == "df" {
			io.WriteString(stdout, "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sdd1 1000 860 140 86% /\n")
		}
		return nil
	}
	t.Cleanup(func() { runCmd = oldCmd })
	s := Store{Root: home}
	now := time.Date(2026, 10, 5, 10, 0, 0, 0, time.Local) // Monday
	for _, n := range []string{"belebob", "guest", "old"} {
		if _, err := s.Add(n, "mailru", testURL+n); err != nil {
			t.Fatal(err)
		}
	}
	g, _ := s.Get("guest")
	g.Expires = now.Add(50 * time.Hour)
	s.Save(g)
	o, _ := s.Get("old")
	o.Paused = true
	s.Save(o)
	os.MkdirAll(s.stateDir("belebob"), 0o700)
	writeFile(t, s.trafficPath("belebob"), `{"days":{"2026-09-27":{"down":9000000000,"up":1},`+
		`"2026-09-28":{"down":1000000000,"up":100000000},"2026-09-30":{"down":3000000000,"up":200000000},"2026-10-05":{"down":7,"up":7}}}`)
	s.logEvents([]event{
		{At: time.Date(2026, 9, 27, 12, 0, 0, 0, time.Local), Level: "FAIL"}, // before the week
		{At: time.Date(2026, 9, 29, 12, 0, 0, 0, time.Local), Level: "FAIL"},
		{At: time.Date(2026, 9, 29, 12, 5, 0, 0, time.Local), Level: "ok"},
		{At: time.Date(2026, 10, 2, 8, 0, 0, 0, time.Local), Level: "warn"},
		{At: time.Date(2026, 10, 3, 8, 0, 0, 0, time.Local), Level: "info"},
	})
	from, to := lastWeek(now)
	got := weeklyReport(s, langRU, tr(langRU, "rep.title", from.Format("02.01"), to.AddDate(0, 0, -1).Format("02.01")), from, to, now)
	for _, want := range []string{
		"📰 <b>Неделя 28.09–04.10</b>",
		"• <b>belebob</b>: ↓ 4.0 GB ↑ 300 MB, больше всего 30.09 (3.2 GB)",
		"• <b>guest</b>: трафика не было",
		"• <b>old</b> (на паузе): трафика не было",
		"Всего: ↓ 4.0 GB ↑ 300 MB",
		"<b>Уведомления</b>: проблем 1, предупреждений 1, восстановлений 1",
		"<b>Заканчивается доступ</b>\n• guest: " + g.Expires.Local().Format("02.01 15:04") + " (через 2 дн)",
		"<b>Сервер</b>: диск / занят на 86%, работает 14 дн",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "%!") || rawIDRe.MatchString(got) {
		t.Errorf("report:\n%s", got)
	}
	// Without the panel, nobody's traffic is counted: say why.
	os.Remove(s.trafficPath("belebob"))
	if got := weeklyReport(s, langEN, "x", from, to, now); !strings.Contains(got, "counted by the web panel") {
		t.Errorf("no traffic files:\n%s", got)
	}
}

func TestReportFromTheBot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	s := Store{Root: home}
	s.saveBotConfig(botConfig{Token: testToken, Chat: 42})
	f := newFakeTG(t)
	c, _ := s.loadBotConfig()
	b := newBot(s, c)
	b.handle(msg(1, 42, "/report"))
	if m := f.messages(); len(m) == 0 || !strings.Contains(m[len(m)-1].Text, "Последние 7 дней") {
		t.Fatalf("/report: %+v", m)
	}
	// Due: sent once and remembered; switched off: not sent.
	monday := time.Date(2026, 10, 12, 10, 0, 0, 0, time.Local)
	s.reportSent(monday.AddDate(0, 0, -7))
	n := len(f.messages())
	b.sendReport(monday)
	b.sendReport(monday.Add(time.Hour))
	if got := len(f.messages()) - n; got != 1 {
		t.Errorf("%d reports sent, want 1", got)
	}
	b.handle(press(2, 42, "mute:report", time.Now()))
	if c, _ := s.loadBotConfig(); strings.Join(c.Mute, ",") != "report" {
		t.Errorf("mute = %v", c.Mute)
	}
	n = len(f.messages())
	b.sendReport(monday.AddDate(0, 0, 7))
	if len(f.messages()) != n {
		t.Error("a report sent with reports off")
	}
}

func TestExpiryWarning(t *testing.T) {
	fakeHost(t, hostState{module: true, worldUp: true, nodeStart: "2026-09-29T17:00:05Z", dfUsed: "50",
		cron: "* * * * * reflux heal\n"})
	if err := run([]string{"expire", "phone", "2d"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	run([]string{"doctor"}, nil, &out)
	if !strings.Contains(out.String(), "warn  access of phone ends ") || !strings.Contains(out.String(), "h): extend it — the bot's access screen, or reflux expire phone 30d") {
		t.Errorf("doctor:\n%s", out.String())
	}
	if category("expiry:phone") != "nodes" {
		t.Error("expiry alerts are not in the clients group")
	}
	run([]string{"expire", "phone", "30d"}, nil, io.Discard)
	out.Reset()
	run([]string{"doctor"}, nil, &out)
	if strings.Contains(out.String(), "access of phone ends") {
		t.Errorf("still warned after extending:\n%s", out.String())
	}
}
