package main

import (
	"sync"
	"testing"
	"time"
)

func TestMinuteAverages(t *testing.T) {
	var a minuteAgg
	base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	var got []hostPoint
	for i := 0; i < 30; i++ { // 2.5 minutes of 5 s samples
		p := hostPoint{At: base.Add(time.Duration(i) * 5 * time.Second), CPU: float64(i / 12 * 10), Load5: 1}
		if avg, ok := a.add(p); ok {
			got = append(got, avg)
		}
	}
	if len(got) != 2 {
		t.Fatalf("%d minutes, want 2: %+v", len(got), got)
	}
	for i, want := range []float64{0, 10} {
		if !got[i].At.Equal(base.Add(time.Duration(i)*time.Minute)) || got[i].CPU != want || got[i].Load5 != 1 {
			t.Errorf("minute %d = %+v, want CPU %v", i, got[i], want)
		}
	}
}

func TestBucketed(t *testing.T) {
	from := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	var ps []hostPoint
	for i := 0; i < 12; i++ {
		ps = append(ps, hostPoint{At: from.Add(time.Duration(i) * 5 * time.Second), CPU: float64(i)})
	}
	got := bucketed(ps, from, 20*time.Second)
	if len(got) != 3 {
		t.Fatalf("%d buckets, want 3: %+v", len(got), got)
	}
	if got[0].CPU != 1.5 || got[2].CPU != 9.5 || !got[2].At.Equal(ps[11].At) {
		t.Errorf("buckets = %+v", got)
	}
	if len(bucketed(nil, from, time.Second)) != 0 {
		t.Error("buckets out of nothing")
	}
}

func TestHistoryJoinsMinutesAndSamples(t *testing.T) {
	now := time.Now()
	m := newSampler(Store{Root: t.TempDir()}, &sync.Mutex{})
	for i := 30 * 60; i > 0; i-- { // 30 hours of minutes, the last hour too
		m.minutes = append(m.minutes, hostPoint{At: now.Add(-time.Duration(i) * time.Minute), CPU: 50})
	}
	for i := 719; i >= 0; i-- { // an hour of samples
		m.host = append(m.host, hostPoint{At: now.Add(-time.Duration(i) * sampleEvery), CPU: 10})
	}
	for _, c := range []struct {
		r        string
		min, max int
		oldCPU   float64 // the oldest point's
	}{
		{"15m", 170, 181, 10},
		{"1h", 230, 241, 10},
		{"6h", 230, 241, 50},
		{"24h", 230, 241, 50},
		{"bogus", 230, 241, 10},
	} {
		h := m.history(c.r, now)
		if len(h) < c.min || len(h) > c.max {
			t.Errorf("%s: %d points", c.r, len(h))
			continue
		}
		if h[0].CPU != c.oldCPU || h[len(h)-1].CPU != 10 {
			t.Errorf("%s: from %v to %v", c.r, h[0].CPU, h[len(h)-1].CPU)
		}
		if span := rangeSpan(c.r); now.Sub(h[0].At) > span {
			t.Errorf("%s starts %v ago", c.r, now.Sub(h[0].At))
		}
	}
}

func TestHistorySurvivesRestarts(t *testing.T) {
	s := Store{Root: t.TempDir()}
	m := newSampler(s, &sync.Mutex{})
	now := time.Now()
	m.minutes = []hostPoint{
		{At: now.Add(-30 * time.Hour), CPU: 1}, // too old to keep
		{At: now.Add(-2 * time.Hour), CPU: 2, Load15: 0.5},
		{At: now.Add(-time.Minute), CPU: 3},
	}
	m.saveHistory(false)
	m.minutes = append(m.minutes, hostPoint{At: now, CPU: 4})
	m.saveHistory(false) // too soon: not written
	n := newSampler(s, &sync.Mutex{})
	n.loadHistory()
	if len(n.minutes) != 2 || n.minutes[0].CPU != 2 || n.minutes[0].Load15 != 0.5 {
		t.Fatalf("loaded %+v", n.minutes)
	}
	m.saveHistory(true)
	n = newSampler(s, &sync.Mutex{})
	n.loadHistory()
	if len(n.minutes) != 3 {
		t.Errorf("forced save: loaded %d minutes", len(n.minutes))
	}
}
