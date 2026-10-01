package main

import (
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strconv"
	"time"
)

// The server page as a dashboard: up/down/total blocks, big figures,
// charts over a chosen range, dials, disks and tables.

// statBlock is an up / down / total block.
type statBlock struct {
	Title           string
	Up, Down, Total int
}

// bigStat is a big figure with a line under it.
type bigStat struct {
	Label, Value, Sub, Level string
}

type chartView struct {
	Title  string
	SVG    template.HTML
	Legend []legendEntry
}

type dial struct {
	Label string
	SVG   template.HTML
}

type rangeLink struct {
	Key, Label string
	On         bool
}

type dashData struct {
	Ranges     []rangeLink
	Range      string
	Ready      bool
	Blocks     []statBlock
	Stats      []bigStat
	Main       chartView // the CPU, wide
	Charts     []chartView
	Dials      []dial
	Now        hostNow
	Containers []containerLive
}

func (w *webServer) server(r *http.Request) (string, pageData, error) {
	l := w.lang()
	rng := r.URL.Query().Get("r")
	if !slices.Contains(chartRanges, rng) {
		rng = "1h"
	}
	now := time.Now()
	h, _ := w.stats.snapshot()
	d := dashData{Range: rng, Now: h, Containers: h.Containers, Ready: !h.Point.At.IsZero()}
	for _, k := range chartRanges {
		d.Ranges = append(d.Ranges, rangeLink{Key: k, Label: tr(l, "web.range."+k), On: k == rng})
	}

	// Up / down / total.
	tunnels := statBlock{Title: tr(l, "web.block.tunnels"), Total: 2}
	if st, err := readEgressStatus(); err == nil {
		for _, ok := range []bool{st.WorldOK, st.RUOK} {
			if ok {
				tunnels.Up++
			}
		}
	}
	tunnels.Down = tunnels.Total - tunnels.Up
	clients, _ := w.s.List()
	online := 0
	active := 0
	for _, v := range viewClients(w.s, clients) {
		if v.active {
			active++
		}
		if v.status != nil && v.status.online {
			online++
		}
	}
	people := statBlock{Title: tr(l, "web.block.clients"), Up: online, Total: len(clients), Down: len(clients) - online}
	boxes := statBlock{Title: tr(l, "web.block.containers"), Up: len(h.Containers), Total: max(1+active, len(h.Containers))}
	boxes.Down = boxes.Total - boxes.Up
	d.Blocks = []statBlock{tunnels, people, boxes}

	// Big figures.
	p := h.Point
	temp, tempSub := "—", ""
	if len(h.Temps) > 0 {
		temp, tempSub = fmt.Sprintf("%.0f °C", h.Temps[0].C), h.Temps[0].Label
	}
	d.Stats = []bigStat{
		{tr(l, "web.tile.cpu"), fmt.Sprintf("%.0f%%", p.CPU), tr(l, "web.cores", h.CPUs), barLevel(p.CPU)},
		{tr(l, "web.stat.load"), fmt.Sprintf("%.2f", p.Load1), tr(l, "web.stat.load.sub", fmt.Sprintf("%.2f", p.Load5), fmt.Sprintf("%.2f", p.Load15)), barLevel(100 * p.Load1 / float64(max(h.CPUs, 1)))},
		{tr(l, "web.tile.mem"), fmt.Sprintf("%.0f%%", p.Mem), tr(l, "web.of", humanBytes(h.Mem.Used()), humanBytes(h.Mem.Total)), barLevel(p.Mem)},
		{tr(l, "web.tile.temp"), temp, tempSub, barLevel(p.TempC)},
		{tr(l, "web.tile.uptime"), durationIn(l, h.Uptime), "", "ok"},
	}

	// Charts over the range.
	hist := w.stats.history(rng, now)
	times := make([]time.Time, len(hist))
	col := func(f func(hostPoint) float64) []float64 {
		out := make([]float64, len(hist))
		for i, q := range hist {
			out[i] = f(q)
		}
		return out
	}
	for i, q := range hist {
		times[i] = q.At
	}
	span := rangeSpan(rng)
	// A gap: the panel was down (the minutes are a minute apart).
	gap := max(4*span/chartPoints, 150*time.Second)
	pctUnit := func(v float64) string { return fmt.Sprintf("%.0f%%", v) }
	add := func(id, title string, c lineChart) {
		c.ID, c.Times, c.From, c.To, c.Gap = id, times, now.Add(-span), now, gap
		v := chartView{Title: title, SVG: c.render(), Legend: c.Legend()}
		if d.Main.SVG == "" {
			d.Main = v
		} else {
			d.Charts = append(d.Charts, v)
		}
	}
	add("cpu", tr(l, "web.chart.cpu"), lineChart{Wide: true, Max: 100, Threshold: 80, Fill: true, Unit: pctUnit,
		Series: []chartSeries{{Name: tr(l, "web.tile.cpu"), Values: col(func(q hostPoint) float64 { return q.CPU })}}})
	add("load", tr(l, "web.chart.load"), lineChart{Unit: func(v float64) string { return fmt.Sprintf("%.1f", v) },
		Threshold: float64(h.CPUs),
		Series: []chartSeries{
			{Name: tr(l, "web.load.1"), Values: col(func(q hostPoint) float64 { return q.Load1 })},
			{Name: tr(l, "web.load.5"), Values: col(func(q hostPoint) float64 { return q.Load5 })},
			{Name: tr(l, "web.load.15"), Values: col(func(q hostPoint) float64 { return q.Load15 })},
		}})
	// Traffic in Mbit/s, so the scale's steps are round in those; at
	// least up to 1 Mbit/s, so keepalives stay at the bottom.
	mb := func(v float64) float64 { return v * 8 / 1e6 }
	add("eg", tr(l, "web.chart.egress"), lineChart{Fill: true, Unit: mbitAxis, Least: 1, Series: []chartSeries{
		{Name: tr(l, "web.rx"), Values: col(func(q hostPoint) float64 { return mb(q.EgRx) })},
		{Name: tr(l, "web.tx"), Values: col(func(q hostPoint) float64 { return mb(q.EgTx) })},
	}})
	add("lan", tr(l, "web.chart.lan"), lineChart{Fill: true, Unit: mbitAxis, Least: 1, Series: []chartSeries{
		{Name: tr(l, "web.rx"), Values: col(func(q hostPoint) float64 { return mb(q.LanRx) })},
		{Name: tr(l, "web.tx"), Values: col(func(q hostPoint) float64 { return mb(q.LanTx) })},
	}})
	// The band is where the bot warns (hostwatch.go).
	tempWarn := 85.0
	if len(h.Temps) > 0 {
		tempWarn = w.s.hostLimits().tempWarnAt(h.Temps[0])
	}
	add("temp", tr(l, "web.chart.temp"), lineChart{Min: 20, Max: 100, Threshold: tempWarn, Unit: func(v float64) string { return fmt.Sprintf("%.0f°", v) },
		Series: []chartSeries{{Name: tr(l, "web.tile.temp"), Values: col(func(q hostPoint) float64 { return q.TempC }), Color: "k3"}}})
	add("mem", tr(l, "web.chart.mem"), lineChart{Max: 100, Threshold: 90, Fill: true, Unit: pctUnit,
		Series: []chartSeries{{Name: tr(l, "web.tile.mem"), Values: col(func(q hostPoint) float64 { return q.Mem }), Color: "k2"}}})

	// Dials.
	d.Dials = append(d.Dials,
		dial{tr(l, "web.tile.cpu"), gauge(p.CPU, fmt.Sprintf("%.0f%%", p.CPU))},
		dial{tr(l, "web.tile.mem"), gauge(p.Mem, fmt.Sprintf("%.0f%%", p.Mem))})
	if h.Mem.SwapTotal > 0 {
		sw := 100 * float64(h.Mem.SwapTotal-h.Mem.SwapFree) / float64(h.Mem.SwapTotal)
		d.Dials = append(d.Dials, dial{"Swap", gauge(sw, fmt.Sprintf("%.0f%%", sw))})
	}
	if len(h.Temps) > 0 {
		d.Dials = append(d.Dials, dial{tr(l, "web.tile.temp"), gauge(h.Temps[0].C, fmt.Sprintf("%.0f°", h.Temps[0].C))})
	}
	for _, dk := range h.Disks {
		if dk.Mount == "/" {
			d.Dials = append(d.Dials, dial{tr(l, "web.tile.disk", "/"), gauge(dk.Percent(), fmt.Sprintf("%.0f%%", dk.Percent()))})
		}
	}
	return "server", pageData{Title: tr(l, "web.nav.server"), Active: "server", Refresh: 15, Body: d}, nil
}

// mbitAxis labels a rate in Mbit/s.
func mbitAxis(m float64) string {
	if m < 0.001 { // keepalives
		return "0"
	}
	if m >= 100 {
		return strconv.FormatFloat(m, 'f', 0, 64)
	}
	return strconv.FormatFloat(m, 'g', 3, 64)
}
