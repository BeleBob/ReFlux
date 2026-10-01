package main

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientDays(t *testing.T) {
	s := Store{Root: t.TempDir()}
	writeFile(t, s.trafficPath("phone"), `{"days":{"2026-10-01":{"down":3000000,"up":100},"2026-09-30":{"down":5,"up":0},"2026-08-01":{"down":9,"up":9}}}`)
	m := newSampler(s, &sync.Mutex{})
	now := time.Date(2026, 10, 1, 15, 0, 0, 0, time.Local)
	days, traffic := m.clientDays("phone", now, 30)
	if len(days) != 30 || days[29].Format(time.DateOnly) != "2026-10-01" || days[0].Format(time.DateOnly) != "2026-09-02" {
		t.Fatalf("days %v .. %v", days[0], days[len(days)-1])
	}
	if traffic[29].Down != 3000000 || traffic[28].Down != 5 || traffic[0] != (dayTraffic{}) {
		t.Errorf("traffic %+v", traffic)
	}
}

func TestBarChart(t *testing.T) {
	base := time.Date(2026, 9, 2, 0, 0, 0, 0, time.Local)
	c := barChart{Unit: func(v float64) string { return humanBytes(uint64(v)) }, Least: 1e6}
	down, up := make([]float64, 30), make([]float64, 30)
	for i := range 30 {
		c.Days = append(c.Days, base.AddDate(0, 0, i))
	}
	down[29], up[29], down[10] = 2e9, 1e8, 5e8
	c.Series = []chartSeries{{Name: "↓ down", Values: down}, {Name: "↑ up", Values: up}}
	svg := string(c.render())
	if n := strings.Count(svg, `class="col k1"`); n != 2 {
		t.Errorf("%d received bars, want 2 (empty days have none)", n)
	}
	if n := strings.Count(svg, `class="col k2"`); n != 1 {
		t.Errorf("%d sent bars, want 1", n)
	}
	for _, want := range []string{"<title>01.10: ↓ down 2.0 GB, ↑ up 100 MB</title>", ">01.10<", ">24.09<", ">0 B<", ">3.0 GB<"} {
		if !strings.Contains(svg, want) {
			t.Errorf("chart lacks %q", want)
		}
	}
	if strings.Contains(svg, "NaN") || strings.Contains(svg, `height="-`) {
		t.Errorf("bad geometry:\n%s", svg)
	}
	// Nothing at all: an axis up to 1 MB, no bars.
	for i := range down {
		down[i], up[i] = 0, 0
	}
	if svg := string(c.render()); strings.Contains(svg, `class="col`) || !strings.Contains(svg, ">1.0 MB<") {
		t.Errorf("empty chart:\n%s", svg)
	}
}

func TestClientPageShowsDays(t *testing.T) {
	wt := newWebTest(t, "192.168.1.50")
	if _, err := wt.s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(wt.s.stateDir("phone"), 0o700)
	today := time.Now().Format(time.DateOnly)
	writeFile(t, wt.s.trafficPath("phone"), `{"days":{"`+today+`":{"down":1500000000,"up":20000000}}}`)
	rec := wt.do("GET", "/c/phone", nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `class="col k1"`) || !strings.Contains(body, "сегодня 1.5 GB · за 30 дней 1.5 GB") {
		t.Errorf("%d, the client page lacks its days:\n%s", rec.Code, body)
	}
}

func TestRateChart(t *testing.T) {
	now := time.Now()
	rates := []ratePoint{{At: now.Add(-20 * time.Second), Down: 1e5, Up: 0}, {At: now.Add(-10 * time.Second), Down: 1.25e6, Up: 2e4}}
	svg, legend := rateChart(langRU, rates, now)
	if !strings.Contains(string(svg), `class="series k1"`) || !strings.Contains(string(svg), `class="series k2"`) {
		t.Errorf("no lines:\n%s", svg)
	}
	if legend[0].Name != "↓ к клиенту" || legend[0].Last != "10" || legend[0].Max != "10" || legend[1].Last != "0.16" {
		t.Errorf("legend %+v", legend)
	}
}
