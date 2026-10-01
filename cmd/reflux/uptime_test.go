package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestUptimeCounts(t *testing.T) {
	s := Store{Root: t.TempDir()}
	day := time.Date(2026, 10, 1, 12, 0, 0, 0, time.Local)
	worldOK := finding{Key: "world", Level: levelOK}
	worldDown := finding{Key: "world", Level: levelFail}
	ruFallback := finding{Key: "russia", Level: levelWarn}
	ruDown := finding{Key: "russia", Level: levelFail}
	egressDown := finding{Key: "egress", Level: levelFail}
	s.recordUptime([]finding{worldOK}, day.AddDate(0, 0, -70)) // trimmed by the next record
	for i, fs := range [][]finding{
		{worldOK, ruFallback}, // the fallback carries Russia
		{worldOK, ruFallback},
		{worldDown, ruDown},
		{egressDown}, // no tunnel at all
	} {
		if err := s.recordUptime(fs, day.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	s.recordUptime([]finding{worldOK, ruFallback}, day.AddDate(0, 0, -1))
	f := s.readUptime()
	if d := f.Days["2026-10-01"]; d == nil || d.World != (uptimeCount{2, 4}) || d.Russia != (uptimeCount{2, 4}) {
		t.Errorf("today %+v", d)
	}
	if len(f.Days) != 2 {
		t.Errorf("days kept: %d, want 2 (older than %d days go)", len(f.Days), uptimeDays)
	}
	world, russia, days := s.uptimeSpan(day.AddDate(0, 0, -6).Truncate(24*time.Hour), day.AddDate(0, 0, 1))
	if world != (uptimeCount{3, 5}) || russia != (uptimeCount{3, 5}) || len(days) != 2 || days[0].Day.Day() != 1 || days[0].World != 50 {
		t.Errorf("span %v %v %+v", world, russia, days)
	}
	if (uptimeCount{0, 0}).percent() != 0 || (uptimeCount{999, 1000}).percent() != 99.9 {
		t.Error("percent")
	}
}

func TestUptimeOnThePagesAndInTheReport(t *testing.T) {
	wt := newWebTest(t, "192.168.1.50")
	if body := wt.do("GET", "/gateway", nil).Body.String(); !strings.Contains(body, "Данных пока нет") {
		t.Error("gateway page without figures")
	}
	now := time.Now()
	for i := 0; i < 200; i++ {
		fs := []finding{{Key: "world", Level: levelOK}, {Key: "russia", Level: levelOK}}
		if i < 2 {
			fs[0].Level = levelFail
		}
		wt.s.recordUptime(fs, now)
	}
	body := wt.do("GET", "/gateway", nil).Body.String()
	for _, want := range []string{"Мир работал 99.0% времени, Россия — 100.0%", `<i class="warn" style="width:99%"></i></span>99.0%`, now.Format("02.01")} {
		if !strings.Contains(body, want) {
			t.Errorf("gateway page lacks %q", want)
		}
	}
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if got := weeklyReport(wt.s, langRU, "x", day, day.AddDate(0, 0, 1), now); !strings.Contains(got, "<b>Туннели</b>: мир работал 99.0% времени, Россия — 100.0%") {
		t.Errorf("report:\n%s", got)
	}
}

func TestTrafficInTheBots(t *testing.T) {
	s, f, owner, cb := bots(t)
	c, _ := s.Add("dima", "mailru", testURL)
	c.Telegram = &TGAccount{ID: 101, Username: "dima"}
	s.Save(c)
	os.MkdirAll(s.stateDir("dima"), 0o700)
	now := time.Now()
	earlier := now.AddDate(0, 0, -1)
	days := fmt.Sprintf(`{"days":{%q:{"down":2000000000,"up":30000000},%q:{"down":5000000,"up":1000}}}`, now.Format(time.DateOnly), earlier.Format(time.DateOnly))
	if earlier.Month() != now.Month() {
		days = fmt.Sprintf(`{"days":{%q:{"down":2000000000,"up":30000000}}}`, now.Format(time.DateOnly))
	}
	writeFile(t, s.trafficPath("dima"), days)
	today, month, ok := s.trafficNow("dima", now)
	if !ok || today.Down != 2000000000 || month.Down < today.Down {
		t.Fatalf("today %+v month %+v %v", today, month, ok)
	}
	// The owner's card (with a running node, as the bot sees it).
	if sc := owner.clientScreen("dima"); !strings.Contains(sc.text, "Сегодня ↓2.0 GB ↑30 MB · за месяц ↓"+humanBytes(month.Down)) {
		t.Errorf("owner's card:\n%s", sc.text)
	}
	cb.handle(personMsg(1, dima, "/start"))
	if m := f.to(101); !strings.Contains(m[len(m)-1].Text, "За месяц: ↓ "+humanBytes(month.Down)) {
		t.Errorf("client bot: %q", m[len(m)-1].Text)
	}
	if _, _, ok := s.trafficNow("nobody", now); ok {
		t.Error("traffic for a client without a file")
	}
}
