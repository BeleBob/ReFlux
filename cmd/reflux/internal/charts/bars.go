package charts

import (
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"
)

// Bars is a stacked bar chart by day: it stacks its series bottom-up for each day.
type Bars struct {
	Days   []time.Time
	Series []Series
	Unit   func(float64) string
	Least  float64 // the scale's least top
}

func (c Bars) Render() template.HTML {
	chW, chH, plotL := Line{}.size()
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
	lo, hi, step := NiceScale(0, top, 4)
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
				x, y(base+v), slot*0.7, y(base)-y(base+v), Line{}.color(j, s))
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
