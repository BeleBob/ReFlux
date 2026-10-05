package main

import (
	"sync"
	"time"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/host"
)

// Warnings about the server itself: the CPU temperature, and a CPU busy
// for a long time (the channels slow down). The bot samples the CPU
// between its checks; `reflux doctor` alone sees only the temperature.

const (
	cpuBusyFor     = 10 * time.Minute
	defaultCPUWarn = 90 // percent
	defaultTemp    = 80 // °C, for a sensor without its own limit
	tempHysteresis = 5  // °C under the limit before a warning clears
)

// hostLimits are the owner's thresholds (the bot's settings), with the
// defaults: the sensor's own limit for the temperature.
type hostLimits struct {
	TempWarn int // °C; 0: the sensor's
	CPUWarn  int // percent
}

func (s Store) hostLimits() hostLimits {
	c, _ := s.loadBotConfig()
	l := hostLimits{TempWarn: c.TempWarn, CPUWarn: c.CPUWarn}
	if l.CPUWarn <= 0 {
		l.CPUWarn = defaultCPUWarn
	}
	return l
}

// tempWarnAt is where a warning starts for t.
func (l hostLimits) tempWarnAt(t host.Sensor) float64 {
	switch {
	case l.TempWarn > 0:
		return float64(l.TempWarn)
	case t.High > 0:
		return t.High
	}
	return defaultTemp
}

// tempFailAt is where it becomes a problem: near the sensor's critical
// point, where the CPU slows itself down or shuts off.
func (l hostLimits) tempFailAt(t host.Sensor) float64 {
	at := 95.0
	if t.Crit > 0 {
		at = t.Crit - 5
	}
	return max(at, l.tempWarnAt(t)+5)
}

var hostWatch struct {
	sync.Mutex
	prevCPU   host.CPUTimes
	prevProcs map[int]host.Proc
	prevAt    time.Time
	measured  bool      // percent covers an interval
	percent   float64   // over the last interval
	top       string    // the busiest process over it
	busySince time.Time // zero: under the limit
	hot       bool      // a temperature warning is on
}

// sampleCPU measures the CPU since the last call (the bot calls it before
// each check) and tracks how long it has been at or over limit percent.
func sampleCPU(now time.Time, limit int) {
	cpu, err := host.ReadCPU()
	if err != nil {
		return
	}
	procs := host.ReadProcs()
	w := &hostWatch
	w.Lock()
	defer w.Unlock()
	if !w.prevAt.IsZero() && now.After(w.prevAt) {
		w.measured = true
		w.percent = w.prevCPU.Percent(cpu)
		w.top = ""
		if top := host.TopProcs(w.prevProcs, procs, w.prevCPU, cpu, 1); len(top) > 0 {
			w.top = top[0].Name
		}
		switch {
		case w.percent < float64(limit):
			w.busySince = time.Time{}
		case w.busySince.IsZero():
			w.busySince = w.prevAt
		}
	}
	w.prevCPU, w.prevProcs, w.prevAt = cpu, procs, now
}

// heat checks the CPU temperature and, when the bot samples it, the CPU.
func (d *doctor) heat(s Store) {
	lim := s.hostLimits()
	w := &hostWatch
	w.Lock()
	defer w.Unlock()
	if temps := host.ReadTemps(); len(temps) > 0 {
		t := temps[0]
		warnAt, failAt := lim.tempWarnAt(t), lim.tempFailAt(t)
		switch {
		case t.C >= warnAt:
			w.hot = true
		case t.C < warnAt-tempHysteresis:
			w.hot = false
		}
		switch {
		case t.C >= failAt:
			d.fail("temp", "temp.crit", t.C, failAt)
		case w.hot:
			d.warn("temp", "temp.hot", t.C, warnAt)
		default:
			d.ok("temp", "temp.ok", t.C, t.Label)
		}
	}
	if !w.measured {
		return
	}
	if !w.busySince.IsZero() && w.prevAt.Sub(w.busySince) >= cpuBusyFor {
		top := w.top
		if top == "" {
			top = "—"
		}
		d.warn("cpu", "cpu.busy", w.percent, durationPhrase(w.prevAt.Sub(w.busySince)), top)
		return
	}
	d.ok("cpu", "cpu.ok", w.percent)
}
