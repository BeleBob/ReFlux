package main

import (
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"
)

// A client's traffic by day as stacked bars, drawn on the server like the
// line charts (webcharts.go).

const trafficChartDays = 30

// barChart stacks its series bottom-up for each day.
type barChart struct {
	Days   []time.Time
	Series []chartSeries
	Unit   func(float64) string
	Least  float64 // the scale's least top
}

func (c barChart) render() template.HTML {
	chW, chH, plotL := lineChart{}.size()
	plotL += 26 // room for "1.5 GB", phones' larger labels too
	plotR, plotB, xLabelY, yLabX := chW-10, chH-32, chH-10, plotL-6
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %.0f %.0f" role="img">`, chW, chH)
	n := len(c.Days)
	if n == 0 {
		fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" class="empty" text-anchor="middle">…</text></svg>`, chW/2, chH/2)
		return template.HTML(b.String())
	}
	top := c.Least
	for i := range c.Days {
		sum := 0.0
		for _, s := range c.Series {
			sum += s.Values[i]
		}
		top = math.Max(top, sum*1.1)
	}
	lo, hi, step := niceScale(0, top, 4)
	y := func(v float64) float64 { return plotB - (math.Min(v, hi)-lo)/(hi-lo)*(plotB-plotT) }
	for k := 0; lo+float64(k)*step <= hi+step/2; k++ {
		v := lo + float64(k)*step
		fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f" class="grid"/><text x="%.1f" y="%.1f" class="axis" text-anchor="end">%s</text>`,
			plotL, plotR, y(v), y(v), yLabX, y(v)+4, template.HTMLEscapeString(c.Unit(v)))
	}
	slot := (plotR - plotL) / float64(n)
	for i, day := range c.Days {
		x := plotL + float64(i)*slot + slot*0.15
		var tip []string
		base := 0.0
		b.WriteString("<g>")
		for j, s := range c.Series {
			v := s.Values[i]
			tip = append(tip, s.Name+" "+c.Unit(v))
			if v <= 0 {
				continue
			}
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" class="col %s"/>`,
				x, y(base+v), slot*0.7, y(base)-y(base+v), lineChart{}.color(j, s))
			base += v
		}
		// A hover tip for the day's column, no script needed.
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" class="hit"><title>%s: %s</title></rect></g>`,
			plotL+float64(i)*slot, plotT, slot, plotB-plotT, day.Format("02.01"), template.HTMLEscapeString(strings.Join(tip, ", ")))
		// A label a week, ending today's at the right edge.
		switch {
		case i == n-1:
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="end">%s</text>`, plotR, xLabelY, day.Format("02.01"))
		case (n-1-i)%7 == 0:
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="axis" text-anchor="middle">%s</text>`, x+slot*0.35, xLabelY, day.Format("02.01"))
		}
	}
	b.WriteString("</svg>")
	return template.HTML(b.String())
}

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
func (w *webServer) trafficChart(l lang, name string, now time.Time) (template.HTML, []legendEntry) {
	days, traffic := w.stats.clientDays(name, now, trafficChartDays)
	down, up := make([]float64, len(days)), make([]float64, len(days))
	var sumDown, sumUp uint64
	for i, t := range traffic {
		down[i], up[i] = float64(t.Down), float64(t.Up)
		sumDown, sumUp = sumDown+t.Down, sumUp+t.Up
	}
	bytesUnit := func(v float64) string { return humanBytes(uint64(math.Round(v))) }
	c := barChart{Days: days, Unit: bytesUnit, Least: 1e6, Series: []chartSeries{
		{Name: "↓ " + tr(l, "web.down"), Values: down},
		{Name: "↑ " + tr(l, "web.up"), Values: up},
	}}
	last := traffic[len(traffic)-1]
	legend := []legendEntry{
		{Name: c.Series[0].Name, Color: "k1", Last: humanBytes(last.Down), Max: humanBytes(sumDown)},
		{Name: c.Series[1].Name, Color: "k2", Last: humanBytes(last.Up), Max: humanBytes(sumUp)},
	}
	return c.render(), legend
}

// rateChart is the client's speed over the last hour, in Mbit/s.
func rateChart(l lang, rates []ratePoint, now time.Time) (template.HTML, []legendEntry) {
	c := lineChart{ID: "rate", From: now.Add(-time.Hour), To: now, Gap: time.Minute,
		Unit: mbitAxis, Least: 1, Fill: true}
	down, up := make([]float64, len(rates)), make([]float64, len(rates))
	for i, p := range rates {
		c.Times = append(c.Times, p.At)
		down[i], up[i] = p.Down*8/1e6, p.Up*8/1e6
	}
	c.Series = []chartSeries{{Name: "↓ " + tr(l, "web.down"), Values: down}, {Name: "↑ " + tr(l, "web.up"), Values: up}}
	return c.render(), c.Legend()
}
