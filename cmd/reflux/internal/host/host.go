// Package host reads the host's vital signs from /proc and /sys (Linux).
package host

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The host's vital signs, read from /proc and /sys (Linux): what the web
// panel and the bot show about the server itself. The roots are variables
// so tests can point them at a fake tree.
var (
	ProcRoot = "/proc"
	SysRoot  = "/sys"
)

// LANIface is the host's uplink to the home network; EgressBridge carries
// all of the egress container's traffic (the tunnels and the direct
// carriers).
var (
	LANIface     = "enp1s0"
	EgressBridge = "reflux0"
)

func ReadLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n"), nil
}

func ReadUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// CPUTimes is the host's CPU time so far: busy and total, in ticks.
type CPUTimes struct{ busy, total uint64 }

func ReadCPU() (CPUTimes, error) {
	lines, err := ReadLines(filepath.Join(ProcRoot, "stat"))
	if err != nil {
		return CPUTimes{}, err
	}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var t CPUTimes
		for i, v := range f[1:] {
			n, _ := strconv.ParseUint(v, 10, 64)
			if i >= 7 { // guest time is counted in user time already
				break
			}
			t.total += n
			if i != 3 && i != 4 { // idle, iowait
				t.busy += n
			}
		}
		return t, nil
	}
	return CPUTimes{}, fmt.Errorf("no cpu line in %s/stat", ProcRoot)
}

// Percent is how busy the CPU was between two readings.
func (t CPUTimes) Percent(next CPUTimes) float64 {
	dt := float64(next.total - t.total)
	if next.total <= t.total || next.busy < t.busy {
		return 0
	}
	return 100 * float64(next.busy-t.busy) / dt
}

// Memory is RAM and swap, in bytes.
type Memory struct {
	Total, Available, SwapTotal, SwapFree uint64
}

func (m Memory) Used() uint64 { return m.Total - m.Available }

func (m Memory) Percent() float64 {
	if m.Total == 0 {
		return 0
	}
	return 100 * float64(m.Used()) / float64(m.Total)
}

func ReadMemory() (Memory, error) {
	lines, err := ReadLines(filepath.Join(ProcRoot, "meminfo"))
	if err != nil {
		return Memory{}, err
	}
	var m Memory
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		kb, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			m.Total = kb * 1024
		case "MemAvailable:":
			m.Available = kb * 1024
		case "SwapTotal:":
			m.SwapTotal = kb * 1024
		case "SwapFree:":
			m.SwapFree = kb * 1024
		}
	}
	if m.Total == 0 {
		return m, fmt.Errorf("no MemTotal in %s/meminfo", ProcRoot)
	}
	return m, nil
}

// ReadLoad is the 1, 5 and 15 minute load averages.
func ReadLoad() ([3]float64, error) {
	var l [3]float64
	lines, err := ReadLines(filepath.Join(ProcRoot, "loadavg"))
	if err != nil || len(lines) == 0 {
		return l, fmt.Errorf("loadavg: %v", err)
	}
	f := strings.Fields(lines[0])
	for i := 0; i < 3 && i < len(f); i++ {
		l[i], _ = strconv.ParseFloat(f[i], 64)
	}
	return l, nil
}

func ReadUptime() (time.Duration, error) {
	lines, err := ReadLines(filepath.Join(ProcRoot, "uptime"))
	if err != nil || len(lines) == 0 {
		return 0, fmt.Errorf("uptime: %v", err)
	}
	s, err := strconv.ParseFloat(strings.Fields(lines[0])[0], 64)
	return time.Duration(s * float64(time.Second)), err
}

// CPUCount is the number of CPUs the kernel lists.
func CPUCount() int {
	lines, _ := ReadLines(filepath.Join(ProcRoot, "stat"))
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "cpu") && len(l) > 3 && l[3] >= '0' && l[3] <= '9' {
			n++
		}
	}
	return max(n, 1)
}

// Sensor is a temperature, in °C.
type Sensor struct {
	Label string
	C     float64
	// High and Crit are the sensor's own limits (0: none): coretemp's
	// max, where it warns, and crit, where the CPU slows itself down.
	High, Crit float64
}

// ReadTemps reads the CPU sensors (coretemp, k10temp, zenpower) and, when
// there are none, the ACPI thermal zone. The package or Tctl reading comes
// first.
func ReadTemps() []Sensor {
	var cpu, other []Sensor
	dirs, _ := filepath.Glob(filepath.Join(SysRoot, "class/hwmon/hwmon*"))
	for _, d := range dirs {
		name, _ := os.ReadFile(filepath.Join(d, "name"))
		chip := strings.TrimSpace(string(name))
		inputs, _ := filepath.Glob(filepath.Join(d, "temp*_input"))
		sort.Strings(inputs)
		for _, in := range inputs {
			v, err := ReadUint(in)
			if err != nil {
				continue
			}
			stem := strings.TrimSuffix(in, "_input")
			label, _ := os.ReadFile(stem + "_label")
			s := Sensor{Label: strings.TrimSpace(string(label)), C: float64(v) / 1000}
			if v, err := ReadUint(stem + "_max"); err == nil {
				s.High = float64(v) / 1000
			}
			if v, err := ReadUint(stem + "_crit"); err == nil {
				s.Crit = float64(v) / 1000
			}
			if s.Label == "" {
				s.Label = chip
			}
			switch chip {
			case "coretemp", "k10temp", "zenpower":
				cpu = append(cpu, s)
			default:
				other = append(other, s)
			}
		}
	}
	sort.SliceStable(cpu, func(i, j int) bool {
		return strings.HasPrefix(cpu[i].Label, "Package") || cpu[i].Label == "Tctl"
	})
	if len(cpu) > 0 {
		return cpu
	}
	return other
}

// IfaceBytes is how much an interface received and sent so far.
func IfaceBytes(iface string) (rx, tx uint64, err error) {
	dir := filepath.Join(SysRoot, "class/net", iface, "statistics")
	if rx, err = ReadUint(filepath.Join(dir, "rx_bytes")); err != nil {
		return 0, 0, err
	}
	tx, err = ReadUint(filepath.Join(dir, "tx_bytes"))
	return rx, tx, err
}

// EgressBytes is what the egress received (from the internet, the
// tunnels and the carriers) and sent, seen from the host's bridge: what
// the bridge transmits goes into the container.
func EgressBytes() (rx, tx uint64, err error) {
	bridgeRx, bridgeTx, err := IfaceBytes(EgressBridge)
	return bridgeTx, bridgeRx, err
}

// Proc is a process's CPU time so far, in ticks.
type Proc struct {
	PID   int
	Name  string
	Ticks uint64
}

// ReadProcs reads every process's name and CPU time.
func ReadProcs() map[int]Proc {
	out := map[int]Proc{}
	dirs, _ := os.ReadDir(ProcRoot)
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(ProcRoot, d.Name(), "stat"))
		if err != nil {
			continue
		}
		// pid (comm) state ppid ... utime(14) stime(15): comm may hold
		// spaces and parentheses, so split after the last ')'.
		s := string(b)
		open, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if open < 0 || end < open {
			continue
		}
		f := strings.Fields(s[end+1:])
		if len(f) < 13 {
			continue
		}
		ut, _ := strconv.ParseUint(f[11], 10, 64)
		st, _ := strconv.ParseUint(f[12], 10, 64)
		out[pid] = Proc{PID: pid, Name: s[open+1 : end], Ticks: ut + st}
	}
	return out
}

// ProcUse is a process's share of the host's CPU between two readings.
type ProcUse struct {
	Name    string
	PID     int
	Percent float64 // of the whole host: 100 is every CPU busy
}

// TopProcs ranks the processes by the CPU they took between before and
// after, grouping by name (qbittorrent-nox, dockerd, ...).
func TopProcs(before, after map[int]Proc, cpu CPUTimes, cpuNext CPUTimes, n int) []ProcUse {
	dt := float64(cpuNext.total - cpu.total)
	if dt <= 0 {
		return nil
	}
	byName := map[string]*ProcUse{}
	for pid, p := range after {
		prev, ok := before[pid]
		if !ok || p.Ticks < prev.Ticks {
			continue
		}
		u := byName[p.Name]
		if u == nil {
			u = &ProcUse{Name: p.Name, PID: pid}
			byName[p.Name] = u
		}
		u.Percent += 100 * float64(p.Ticks-prev.Ticks) / dt
	}
	var out []ProcUse
	for _, u := range byName {
		if u.Percent >= 0.5 {
			out = append(out, *u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Percent > out[j].Percent })
	if len(out) > n {
		out = out[:n]
	}
	return out
}
