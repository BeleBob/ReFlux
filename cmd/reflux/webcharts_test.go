package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNiceScale(t *testing.T) {
	for _, c := range []struct {
		lo, hi, n      float64
		min, max, step float64
	}{
		{0, 100, 4, 0, 100, 20},
		{20, 100, 4, 20, 100, 20},
		{0, 1.15, 4, 0, 1.5, 0.5},
		{0, 0, 4, 0, 1, 0.2},
		{0, 37, 4, 0, 40, 10},
	} {
		min, max, step := niceScale(c.lo, c.hi, int(c.n))
		if fmt.Sprintf("%.4g %.4g %.4g", min, max, step) != fmt.Sprintf("%.4g %.4g %.4g", c.min, c.max, c.step) {
			t.Errorf("niceScale(%v, %v) = %v %v %v, want %v %v %v", c.lo, c.hi, min, max, step, c.min, c.max, c.step)
		}
	}
}

func chartAt(times ...time.Duration) lineChart {
	base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)
	c := lineChart{ID: "t", Unit: func(v float64) string { return fmt.Sprintf("%.0f", v) }, Fill: true}
	s := chartSeries{Name: "a"}
	for i, d := range times {
		c.Times = append(c.Times, base.Add(d))
		s.Values = append(s.Values, float64(10*i))
	}
	c.Series = []chartSeries{s}
	return c
}

func TestLineChartRender(t *testing.T) {
	c := chartAt(0, time.Minute, 2*time.Minute, 3*time.Minute)
	c.Threshold = 25
	svg := string(c.render())
	for _, want := range []string{`class="series k1"`, `fill="url(#g-t-0)"`, `class="zone"`, `class="limit"`, `>12:00<`, `>12:03<`, `>30<`} {
		if !strings.Contains(svg, want) {
			t.Errorf("chart lacks %q:\n%s", want, svg)
		}
	}
	if strings.Contains(svg, "NaN") || strings.Contains(svg, "Inf") {
		t.Errorf("chart has a bad number:\n%s", svg)
	}
	// A fixed axis wider than the data: the line starts in the middle.
	c.From, c.To = c.Times[0].Add(-3*time.Minute), c.Times[3]
	if svg := string(c.render()); !strings.Contains(svg, `d="M342.0,`) {
		t.Errorf("the line does not start at the data's time:\n%s", svg)
	}
	// No data, or one point: no line, no NaN.
	for _, c := range []lineChart{chartAt(), chartAt(0)} {
		if svg := string(c.render()); strings.Contains(svg, "series") || strings.Contains(svg, "NaN") {
			t.Errorf("an empty chart draws:\n%s", svg)
		}
	}
	// All values zero: still a scale.
	z := chartAt(0, time.Minute)
	z.Series[0].Values = []float64{0, 0}
	if svg := string(z.render()); strings.Contains(svg, "NaN") || !strings.Contains(svg, "series") {
		t.Errorf("a flat chart:\n%s", svg)
	}
}

func TestLineChartBreaksAtGaps(t *testing.T) {
	c := chartAt(0, time.Minute, 2*time.Minute, 30*time.Minute, 31*time.Minute, 50*time.Minute)
	c.Gap = 5 * time.Minute
	if got := fmt.Sprint(c.segments()); got != "[[0 3] [3 5] [5 6]]" {
		t.Errorf("segments = %s", got)
	}
	svg := string(c.render())
	if n := strings.Count(svg, `class="series k1"`); n != 2 {
		t.Errorf("%d lines, want 2 (and a dot):\n%s", n, svg)
	}
	if !strings.Contains(svg, `class="dot k1"`) {
		t.Errorf("the lone point is not drawn:\n%s", svg)
	}
	c.Gap = 0
	if got := fmt.Sprint(c.segments()); got != "[[0 6]]" {
		t.Errorf("no gap: segments = %s", got)
	}
}

func TestLegend(t *testing.T) {
	c := chartAt(0, time.Minute, 2*time.Minute)
	c.Series = append(c.Series, chartSeries{Name: "b", Color: "k4"})
	got := c.Legend()
	if len(got) != 2 || got[0] != (legendEntry{"a", "k1", "20", "20"}) || got[1] != (legendEntry{"b", "k4", "—", "—"}) {
		t.Errorf("legend = %+v", got)
	}
}

func TestGauge(t *testing.T) {
	for _, c := range []struct {
		p    float64
		want string
	}{{-5, `level ok" stroke-dasharray="0.00 `}, {50, `level ok`}, {90, `level warn`}, {130, `level FAIL`}} {
		if g := string(gauge(c.p, "<x>")); !strings.Contains(g, c.want) || !strings.Contains(g, "&lt;x&gt;") {
			t.Errorf("gauge(%v) = %s", c.p, g)
		}
	}
}
