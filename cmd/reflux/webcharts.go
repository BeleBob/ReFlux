package main

import (
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"
)

// Charts for the server page, drawn on the server as SVG: the panel runs
// no scripts. A series' colour is a CSS class (k1..k4) that sets --k.

// seriesColors are the lines' colour classes, in order.
var seriesColors = []string{"k1", "k2", "k3", "k4"}

type chartSeries struct {
	Name   string
	Values []float64
	Color  string // colour class; empty picks from seriesColors
}

type lineChart struct {
	ID       string // unique on the page, for the gradients
	Times    []time.Time
	From, To time.Time     // the time axis; zero: the data's own span
	Gap      time.Duration // points farther apart are not joined (0: all are)
	Series   []chartSeries
	Min, Max float64 // fixed scale; Max 0 scales to the data
	Unit     func(float64) string
	// Threshold shades the band above it (0: none).
	Threshold float64
	Fill      bool // shade under the lines
	// Wide draws a larger picture, for a chart twice as wide on the page:
	// the labels come out the same size as on the others.
	Wide bool
}

// legendEntry is a series under its chart: the latest value and the top.
type legendEntry struct {
	Name, Color, Last, Max string
}

const (
	plotT  = 12.0
	xTicks = 5
)

// size is the picture's width and height, and where the plot starts.
func (c lineChart) size() (w, h, left float64) {
	if c.Wide {
		return 960, 300, 72
	}
	return 640, 236, 54
}

// niceNum rounds x to 1, 2, 5 or 10 times a power of ten.
func niceNum(x float64, round bool) float64 {
	if x <= 0 {
		return 1
	}
	exp := math.Floor(math.Log10(x))
	f := x / math.Pow(10, exp)
	var nf float64
	switch {
	case round && f < 1.5, !round && f <= 1:
		nf = 1
	case round && f < 3, !round && f <= 2:
		nf = 2
	case round && f < 7, !round && f <= 5:
		nf = 5
	default:
		nf = 10
	}
	return nf * math.Pow(10, exp)
}

// niceScale spreads [lo, hi] over about n round steps.
func niceScale(lo, hi float64, n int) (min, max, step float64) {
	if hi <= lo {
		hi = lo + 1
	}
	step = niceNum(niceNum(hi-lo, false)/float64(n), true)
	return math.Floor(lo/step) * step, math.Ceil(hi/step) * step, step
}

// render draws the chart.
func (c lineChart) render() template.HTML {
	var b strings.Builder
	chW, chH, plotL := c.size()
	plotR, plotB, xLabelY, yLabX := chW-10, chH-32, chH-10, plotL-6
	class := "chart"
	if c.Wide {
		class += " wide"
	}
	fmt.Fprintf(&b, `<svg class="%s" viewBox="0 0 %.0f %.0f" role="img">`, class, chW, chH)
	n := len(c.Times)
	if n < 2 {
		fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" class="empty" text-anchor="middle">…</text></svg>`, chW/2, chH/2)
		return template.HTML(b.String())
	}
	lo, hi := c.Min, c.Max
	if hi <= 0 {
		for _, s := range c.Series {
			for _, v := range s.Values {
				hi = math.Max(hi, v)
			}
		}
		hi = math.Max(hi*1.15, c.Threshold)
		if hi <= 0 {
			hi = 1
		}
	}
	lo, hi, step := niceScale(lo, hi, 4)
	from, to := c.From, c.To
	if from.IsZero() || !to.After(from) {
		from, to = c.Times[0], c.Times[n-1]
	}
	if !to.After(from) {
		to = from.Add(time.Second)
	}
	y := func(v float64) float64 { return plotB - (math.Min(math.Max(v, lo), hi)-lo)/(hi-lo)*(plotB-plotT) }
	x := func(t time.Time) float64 {
		f := float64(t.Sub(from)) / float64(to.Sub(from))
		return plotL + math.Min(math.Max(f, 0), 1)*(plotR-plotL)
	}

	// Gradients for the fills.
	b.WriteString("<defs>")
	for i, s := range c.Series {
		fmt.Fprintf(&b, `<linearGradient id="g-%s-%d" class="%s" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-opacity=".32"/><stop offset="1" stop-opacity="0"/></linearGradient>`,
			c.ID, i, c.color(i, s))
	}
	b.WriteString("</defs>")

	// Threshold band, grid, axis labels.
	if c.Threshold > 0 && c.Threshold < hi {
		ty := y(c.Threshold)
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" class="zone"/><line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f" class="limit"/>`,
			plotL, plotT, plotR-plotL, ty-plotT, plotL, plotR, ty, ty)
	}
	for k := 0; lo+float64(k)*step <= hi+step/2; k++ {
		v := lo + float64(k)*step
		gy := y(v)
		fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f" class="grid"/><text x="%.1f" y="%.1f" class="axis" text-anchor="end">%s</text>`,
			plotL, plotR, gy, gy, yLabX, gy+4, template.HTMLEscapeString(c.Unit(v)))
	}
	for k := 0; k < xTicks; k++ {
		at := from.Add(time.Duration(k) * to.Sub(from) / (xTicks - 1))
		gx := x(at)
		anchor := "middle"
		switch k {
		case 0:
			anchor = "start"
		case xTicks - 1:
			anchor = "end"
		}
		fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f" class="grid v"/><text x="%.1f" y="%.1f" class="axis" text-anchor="%s">%s</text>`,
			gx, gx, plotT, plotB, gx, xLabelY, anchor, at.Local().Format("15:04"))
	}

	// The lines over their fills, broken where the samples have a gap.
	for i, s := range c.Series {
		for _, seg := range c.segments() {
			pts := make([][2]float64, 0, seg[1]-seg[0])
			for j := seg[0]; j < seg[1] && j < len(s.Values); j++ {
				pts = append(pts, [2]float64{x(c.Times[j]), y(s.Values[j])})
			}
			switch {
			case len(pts) == 0:
			case len(pts) == 1:
				fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="2" class="dot %s"/>`, pts[0][0], pts[0][1], c.color(i, s))
			default:
				d := smoothPath(pts, plotB)
				if c.Fill {
					fmt.Fprintf(&b, `<path d="%s L%.1f,%.1f L%.1f,%.1f Z" class="area" fill="url(#g-%s-%d)"/>`,
						d, pts[len(pts)-1][0], plotB, pts[0][0], plotB, c.ID, i)
				}
				fmt.Fprintf(&b, `<path d="%s" class="series %s"/>`, d, c.color(i, s))
			}
		}
	}
	b.WriteString("</svg>")
	return template.HTML(b.String())
}

// segments splits the samples at the gaps: [from, to) index pairs.
func (c lineChart) segments() [][2]int {
	var out [][2]int
	start := 0
	for j := 1; j < len(c.Times); j++ {
		if c.Gap > 0 && c.Times[j].Sub(c.Times[j-1]) > c.Gap {
			out = append(out, [2]int{start, j})
			start = j
		}
	}
	return append(out, [2]int{start, len(c.Times)})
}

func (c lineChart) color(i int, s chartSeries) string {
	if s.Color != "" {
		return s.Color
	}
	return seriesColors[i%len(seriesColors)]
}

// Legend lists the series with their colours, latest values and tops.
func (c lineChart) Legend() []legendEntry {
	out := make([]legendEntry, len(c.Series))
	for i, s := range c.Series {
		e := legendEntry{Name: s.Name, Color: c.color(i, s), Last: "—", Max: "—"}
		if n := len(s.Values); n > 0 {
			top := s.Values[0]
			for _, v := range s.Values {
				top = math.Max(top, v)
			}
			e.Last, e.Max = c.Unit(s.Values[n-1]), c.Unit(top)
		}
		out[i] = e
	}
	return out
}

// smoothPath draws a Catmull-Rom curve through the points, its control
// points kept inside the plot (down to bottom) so the line never dips
// below zero.
func smoothPath(pts [][2]float64, bottom float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "M%.1f,%.1f", pts[0][0], pts[0][1])
	if len(pts) > 120 { // dense enough: straight segments
		for _, p := range pts[1:] {
			fmt.Fprintf(&b, " L%.1f,%.1f", p[0], p[1])
		}
		return b.String()
	}
	clampY := func(v float64) float64 { return math.Min(math.Max(v, plotT), bottom) }
	for i := 0; i < len(pts)-1; i++ {
		p0, p1, p2 := pts[max(i-1, 0)], pts[i], pts[i+1]
		p3 := pts[min(i+2, len(pts)-1)]
		c1 := [2]float64{p1[0] + (p2[0]-p0[0])/6, clampY(p1[1] + (p2[1]-p0[1])/6)}
		c2 := [2]float64{p2[0] - (p3[0]-p1[0])/6, clampY(p2[1] - (p3[1]-p1[1])/6)}
		fmt.Fprintf(&b, " C%.1f,%.1f %.1f,%.1f %.1f,%.1f", c1[0], c1[1], c2[0], c2[1], p2[0], p2[1])
	}
	return b.String()
}

// gauge draws a 270° dial for a percentage, coloured by level.
func gauge(percent float64, value string) template.HTML {
	const r, sweep = 40.0, 1.5 * math.Pi
	length := r * sweep
	p := math.Min(math.Max(percent, 0), 100) / 100
	// From 135° (bottom left) clockwise to 45° (bottom right).
	sx, sy := 50+r*math.Cos(0.75*math.Pi), 52+r*math.Sin(0.75*math.Pi)
	ex, ey := 50+r*math.Cos(0.25*math.Pi), 52+r*math.Sin(0.25*math.Pi)
	arc := fmt.Sprintf("M%.2f,%.2f A%.0f,%.0f 0 1 1 %.2f,%.2f", sx, sy, r, r, ex, ey)
	return template.HTML(fmt.Sprintf(`<svg class="gauge" viewBox="0 0 100 92" role="img">`+
		`<path d="%s" class="track"/><path d="%s" class="level %s" stroke-dasharray="%.2f %.2f"/>`+
		`<text x="50" y="58" text-anchor="middle" class="gv">%s</text></svg>`,
		arc, arc, barLevel(percent), p*length, length, template.HTMLEscapeString(value)))
}
