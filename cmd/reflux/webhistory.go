package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// The host's history in minutes: the averages of the fine samples, a day
// of them, kept in metrics.json across the panel's restarts, for the
// charts' longer ranges.

const (
	minutesLen   = 24 * 60
	historyEvery = 10 * time.Minute
	chartPoints  = 240 // at most this many points per chart
)

// minuteAgg adds up the samples of the minute under way.
type minuteAgg struct {
	start time.Time
	sum   hostPoint
	n     int
}

// add takes a sample and, once a minute has passed, returns the average
// of the one before.
func (a *minuteAgg) add(p hostPoint) (hostPoint, bool) {
	var avg hostPoint
	done := false
	if a.n > 0 && p.At.Sub(a.start) >= time.Minute {
		avg = a.sum.scaled(1 / float64(a.n))
		avg.At = a.start
		done = true
		a.n, a.sum = 0, hostPoint{}
	}
	if a.n == 0 {
		a.start = p.At.Truncate(time.Minute)
	}
	a.sum = a.sum.plus(p)
	a.n++
	return avg, done
}

func (p hostPoint) plus(q hostPoint) hostPoint {
	return hostPoint{At: p.At, CPU: p.CPU + q.CPU, Mem: p.Mem + q.Mem, Load1: p.Load1 + q.Load1,
		Load5: p.Load5 + q.Load5, Load15: p.Load15 + q.Load15, TempC: p.TempC + q.TempC,
		LanRx: p.LanRx + q.LanRx, LanTx: p.LanTx + q.LanTx, EgRx: p.EgRx + q.EgRx, EgTx: p.EgTx + q.EgTx}
}

func (p hostPoint) scaled(k float64) hostPoint {
	return hostPoint{At: p.At, CPU: p.CPU * k, Mem: p.Mem * k, Load1: p.Load1 * k, Load5: p.Load5 * k,
		Load15: p.Load15 * k, TempC: p.TempC * k, LanRx: p.LanRx * k, LanTx: p.LanTx * k, EgRx: p.EgRx * k, EgTx: p.EgTx * k}
}

// bucketed averages the points in buckets of width w counted from from,
// each average at the time of its bucket's last point.
func bucketed(ps []hostPoint, from time.Time, w time.Duration) []hostPoint {
	var out []hostPoint
	var sum hostPoint
	var last time.Time
	n, cur := 0, int64(-1)
	flush := func() {
		if n > 0 {
			avg := sum.scaled(1 / float64(n))
			avg.At = last
			out = append(out, avg)
		}
	}
	for _, p := range ps {
		if b := int64(p.At.Sub(from) / w); b != cur {
			flush()
			sum, n, cur = hostPoint{}, 0, b
		}
		sum, n, last = sum.plus(p), n+1, p.At
	}
	flush()
	return out
}

// chartRanges are the ranges the server page offers.
var chartRanges = []string{"15m", "1h", "6h", "24h"}

// rangeSpan is how far back a range goes.
func rangeSpan(r string) time.Duration {
	switch r {
	case "15m":
		return 15 * time.Minute
	case "6h":
		return 6 * time.Hour
	case "24h":
		return 24 * time.Hour
	}
	return time.Hour
}

// history returns the host's samples over a range, averaged into at most
// chartPoints buckets: the fine samples for the last hour, the minute
// averages before them.
func (m *sampler) history(r string, now time.Time) []hostPoint {
	span := rangeSpan(r)
	from := now.Add(-span)
	m.mu.Lock()
	fineFrom := now
	if len(m.host) > 0 {
		fineFrom = m.host[0].At
	}
	var src []hostPoint
	for _, p := range m.minutes {
		if p.At.After(from) && p.At.Before(fineFrom) {
			src = append(src, p)
		}
	}
	for _, p := range m.host {
		if p.At.After(from) {
			src = append(src, p)
		}
	}
	m.mu.Unlock()
	return bucketed(src, from, span/chartPoints)
}

func (s Store) metricsPath() string { return filepath.Join(s.Root, "metrics.json") }

// loadHistory reads the minutes kept by the panel's last run.
func (m *sampler) loadHistory() {
	b, err := os.ReadFile(m.s.metricsPath())
	if err != nil {
		return
	}
	var ps []hostPoint
	if err := json.Unmarshal(b, &ps); err != nil {
		log.Printf("web: %s: %v", m.s.metricsPath(), err)
		return
	}
	from := time.Now().Add(-24 * time.Hour)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range ps {
		if p.At.After(from) {
			m.minutes = append(m.minutes, p)
		}
	}
}

// saveHistory writes the minutes, at most every historyEvery unless forced.
func (m *sampler) saveHistory(force bool) {
	m.mu.Lock()
	if !force && time.Since(m.historyAt) < historyEvery {
		m.mu.Unlock()
		return
	}
	m.historyAt = time.Now()
	ps := append([]hostPoint(nil), m.minutes...)
	m.mu.Unlock()
	if err := writeJSON(m.s.metricsPath(), ps); err != nil {
		log.Printf("web: saving the history: %v", err)
	}
}
