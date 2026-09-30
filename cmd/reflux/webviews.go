package main

import (
	"fmt"
	"html/template"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"
)

// measureWindow is how long measureHost watches the host.
var measureWindow = time.Second

// measureHost reads the host twice, d apart, for the rates: what the bot
// and the CLI show, having no sampler of their own.
func measureHost(d time.Duration) hostNow {
	cpu, _ := readCPU()
	procs := readProcs()
	lanRx, lanTx, _ := ifaceBytes(lanIface)
	egRx, egTx, _ := ifaceBytes(egressBridge)
	time.Sleep(d)
	cpu2, _ := readCPU()
	procs2 := readProcs()
	lanRx2, lanTx2, _ := ifaceBytes(lanIface)
	egRx2, egTx2, _ := ifaceBytes(egressBridge)
	mem, _ := readMemory()
	load, _ := readLoad()
	up, _ := readUptime()
	secs := d.Seconds()
	h := hostNow{Load: load, Mem: mem, Uptime: up, CPUs: cpuCount(), Temps: readTemps(), Disks: readDisks(),
		Top: topProcs(procs, procs2, cpu, cpu2, 5)}
	h.Point = hostPoint{At: time.Now(), CPU: cpu.percent(cpu2), Mem: mem.Percent(), Load1: load[0],
		LanRx: rate(lanRx, lanRx2, secs), LanTx: rate(lanTx, lanTx2, secs),
		EgRx: rate(egRx, egRx2, secs), EgTx: rate(egTx, egTx2, secs)}
	if len(h.Temps) > 0 {
		h.Point.TempC = h.Temps[0].C
	}
	return h
}

// tunnel is an egress tunnel as awg sees it.
type tunnel struct {
	Iface     string
	Handshake time.Time
	Rx, Tx    uint64
}

// readTunnels asks the egress for its tunnels' last handshakes and
// traffic (not awg's dump: that holds the private keys).
func readTunnels() []tunnel {
	var hs, tr strings.Builder
	if quiet(&hs, "exec", "reflux-egress", "awg", "show", "all", "latest-handshakes") != nil {
		return nil
	}
	quiet(&tr, "exec", "reflux-egress", "awg", "show", "all", "transfer")
	byIface := map[string]*tunnel{}
	get := func(iface string) *tunnel {
		if byIface[iface] == nil {
			byIface[iface] = &tunnel{Iface: iface}
		}
		return byIface[iface]
	}
	for _, l := range strings.Split(hs.String(), "\n") {
		if f := strings.Fields(l); len(f) == 3 {
			if sec, err := strconv.ParseInt(f[2], 10, 64); err == nil && sec > 0 {
				get(f[0]).Handshake = time.Unix(sec, 0)
			}
		}
	}
	for _, l := range strings.Split(tr.String(), "\n") {
		if f := strings.Fields(l); len(f) == 4 {
			t := get(f[0])
			t.Rx, _ = strconv.ParseUint(f[2], 10, 64)
			t.Tx, _ = strconv.ParseUint(f[3], 10, 64)
		}
	}
	var out []tunnel
	for _, t := range byIface {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Iface > out[j].Iface }) // awg-world, awg-ru
	return out
}

// version is the commit reflux was built from.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if rev == "" {
		return "dev"
	}
	if dirty {
		rev += "+"
	}
	return rev
}

// spark draws values as a small line chart (SVG). top is the value at the
// top edge; 0 scales to the largest value.
func spark(values []float64, top float64) template.HTML {
	const w, h = 240.0, 44.0
	if len(values) < 2 {
		return template.HTML(`<svg class="spark" viewBox="0 0 240 44" preserveAspectRatio="none"></svg>`)
	}
	if top <= 0 {
		for _, v := range values {
			top = max(top, v)
		}
		top = max(top*1.1, 1e-9)
	}
	var pts strings.Builder
	step := w / float64(len(values)-1)
	for i, v := range values {
		y := h - min(v/top, 1)*(h-2) - 1
		fmt.Fprintf(&pts, "%.1f,%.1f ", float64(i)*step, y)
	}
	line := strings.TrimSpace(pts.String())
	return template.HTML(fmt.Sprintf(`<svg class="spark" viewBox="0 0 240 44" preserveAspectRatio="none">`+
		`<polygon points="0,44 %s 240,44" class="area"/><polyline points="%s" class="line"/></svg>`, line, line))
}

// pct is a percentage for a bar's width, 0 to 100.
func pct(v float64) int { return int(min(max(v, 0), 100)) }

// barLevel colours a usage bar.
func barLevel(v float64) string {
	switch {
	case v >= 95:
		return "FAIL"
	case v >= 85:
		return "warn"
	}
	return "ok"
}

// mbit formats a rate in bytes per second as megabits.
func mbit(l lang, bytesPerSec float64) string {
	return tr(l, "web.mbps", bytesPerSec*8/1e6)
}

// ago says how long ago t was, coarsely.
func ago(l lang, t time.Time) string {
	if t.IsZero() {
		return tr(l, "web.never")
	}
	return tr(l, "web.ago", durationIn(l, time.Since(t)))
}
