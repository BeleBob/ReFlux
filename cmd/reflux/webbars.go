package main

import (
	"html/template"
	"math"
	"time"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/charts"
)

// A client's traffic by day as stacked bars, drawn on the server like the
// line charts (internal/charts).

const trafficChartDays = 30

// clientDays returns a client's traffic for the n days up to today, the
// oldest first, zero for days without any.
func (m *sampler) clientDays(name string, now time.Time, n int) ([]time.Time, []dayTraffic) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tf := m.trafficOf(name)
	days, out := make([]time.Time, n), make([]dayTraffic, n)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for i := range n {
		d := today.AddDate(0, 0, i-n+1)
		days[i], out[i] = d, tf.Days[d.Format(time.DateOnly)]
	}
	return days, out
}

// trafficChart is the client page's chart of the last days, and its
// legend: today and the whole span for each direction.
func (w *webServer) trafficChart(l lang, name string, now time.Time) (template.HTML, []charts.LegendEntry) {
	days, traffic := w.stats.clientDays(name, now, trafficChartDays)
	down, up := make([]float64, len(days)), make([]float64, len(days))
	var sumDown, sumUp uint64
	for i, t := range traffic {
		down[i], up[i] = float64(t.Down), float64(t.Up)
		sumDown, sumUp = sumDown+t.Down, sumUp+t.Up
	}
	bytesUnit := func(v float64) string { return humanBytes(uint64(math.Round(v))) }
	c := charts.Bars{Days: days, Unit: bytesUnit, Least: 1e6, Series: []charts.Series{
		{Name: "↓ " + tr(l, "web.down"), Values: down},
		{Name: "↑ " + tr(l, "web.up"), Values: up},
	}}
	last := traffic[len(traffic)-1]
	legend := []charts.LegendEntry{
		{Name: c.Series[0].Name, Color: "k1", Last: humanBytes(last.Down), Max: humanBytes(sumDown)},
		{Name: c.Series[1].Name, Color: "k2", Last: humanBytes(last.Up), Max: humanBytes(sumUp)},
	}
	return c.Render(), legend
}

// rateChart is the client's speed over the last hour, in Mbit/s.
func rateChart(l lang, rates []ratePoint, now time.Time) (template.HTML, []charts.LegendEntry) {
	c := charts.Line{ID: "rate", From: now.Add(-time.Hour), To: now, Gap: time.Minute,
		Unit: charts.MbitAxis, Least: 1, Fill: true}
	down, up := make([]float64, len(rates)), make([]float64, len(rates))
	for i, p := range rates {
		c.Times = append(c.Times, p.At)
		down[i], up[i] = p.Down*8/1e6, p.Up*8/1e6
	}
	c.Series = []charts.Series{{Name: "↓ " + tr(l, "web.down"), Values: down}, {Name: "↑ " + tr(l, "web.up"), Values: up}}
	return c.Render(), c.Legend()
}
