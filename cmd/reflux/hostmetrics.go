package main

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
	procRoot = "/proc"
	sysRoot  = "/sys"
)

// lanIface is the host's uplink to the home network; egressBridge carries
// all of the egress container's traffic (the tunnels and the direct
// carriers).
var (
	lanIface     = "enp1s0"
	egressBridge = "reflux0"
)

func readLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n"), nil
}

func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// cpuTimes is the host's CPU time so far: busy and total, in ticks.
type cpuTimes struct{ busy, total uint64 }

func readCPU() (cpuTimes, error) {
	lines, err := readLines(filepath.Join(procRoot, "stat"))
	if err != nil {
		return cpuTimes{}, err
	}
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		var t cpuTimes
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
	return cpuTimes{}, fmt.Errorf("no cpu line in %s/stat", procRoot)
}

// percent is how busy the CPU was between two readings.
func (t cpuTimes) percent(next cpuTimes) float64 {
	dt := float64(next.total - t.total)
	if next.total <= t.total || next.busy < t.busy {
		return 0
	}
	return 100 * float64(next.busy-t.busy) / dt
}

// memory is RAM and swap, in bytes.
type memory struct {
	Total, Available, SwapTotal, SwapFree uint64
}

func (m memory) Used() uint64 { return m.Total - m.Available }

func (m memory) Percent() float64 {
	if m.Total == 0 {
		return 0
	}
	return 100 * float64(m.Used()) / float64(m.Total)
}

func readMemory() (memory, error) {
	lines, err := readLines(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return memory{}, err
	}
	var m memory
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
		return m, fmt.Errorf("no MemTotal in %s/meminfo", procRoot)
	}
	return m, nil
}

// readLoad is the 1, 5 and 15 minute load averages.
func readLoad() ([3]float64, error) {
	var l [3]float64
	lines, err := readLines(filepath.Join(procRoot, "loadavg"))
	if err != nil || len(lines) == 0 {
		return l, fmt.Errorf("loadavg: %v", err)
	}
	f := strings.Fields(lines[0])
	for i := 0; i < 3 && i < len(f); i++ {
		l[i], _ = strconv.ParseFloat(f[i], 64)
	}
	return l, nil
}

func readUptime() (time.Duration, error) {
	lines, err := readLines(filepath.Join(procRoot, "uptime"))
	if err != nil || len(lines) == 0 {
		return 0, fmt.Errorf("uptime: %v", err)
	}
	s, err := strconv.ParseFloat(strings.Fields(lines[0])[0], 64)
	return time.Duration(s * float64(time.Second)), err
}

// cpuCount is the number of CPUs the kernel lists.
func cpuCount() int {
	lines, _ := readLines(filepath.Join(procRoot, "stat"))
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "cpu") && len(l) > 3 && l[3] >= '0' && l[3] <= '9' {
			n++
		}
	}
	return max(n, 1)
}

// sensor is a temperature, in °C.
type sensor struct {
	Label string
	C     float64
}

// readTemps reads the CPU sensors (coretemp, k10temp, zenpower) and, when
// there are none, the ACPI thermal zone. The package or Tctl reading comes
// first.
func readTemps() []sensor {
	var cpu, other []sensor
	dirs, _ := filepath.Glob(filepath.Join(sysRoot, "class/hwmon/hwmon*"))
	for _, d := range dirs {
		name, _ := os.ReadFile(filepath.Join(d, "name"))
		chip := strings.TrimSpace(string(name))
		inputs, _ := filepath.Glob(filepath.Join(d, "temp*_input"))
		sort.Strings(inputs)
		for _, in := range inputs {
			v, err := readUint(in)
			if err != nil {
				continue
			}
			label, _ := os.ReadFile(strings.TrimSuffix(in, "_input") + "_label")
			s := sensor{Label: strings.TrimSpace(string(label)), C: float64(v) / 1000}
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

// ifaceBytes is how much an interface received and sent so far.
func ifaceBytes(iface string) (rx, tx uint64, err error) {
	dir := filepath.Join(sysRoot, "class/net", iface, "statistics")
	if rx, err = readUint(filepath.Join(dir, "rx_bytes")); err != nil {
		return 0, 0, err
	}
	tx, err = readUint(filepath.Join(dir, "tx_bytes"))
	return rx, tx, err
}

// egressBytes is what the egress received (from the internet, the
// tunnels and the carriers) and sent, seen from the host's bridge: what
// the bridge transmits goes into the container.
func egressBytes() (rx, tx uint64, err error) {
	bridgeRx, bridgeTx, err := ifaceBytes(egressBridge)
	return bridgeTx, bridgeRx, err
}

// disk is a mounted filesystem.
type disk struct {
	Mount       string
	Used, Total uint64
}

func (d disk) Percent() float64 {
	if d.Total == 0 {
		return 0
	}
	return 100 * float64(d.Used) / float64(d.Total)
}

// readDisks lists the real filesystems (df, without memory and container
// ones).
func readDisks() []disk {
	var b strings.Builder
	runCmd(&b, "df", "-P", "-k", "-x", "tmpfs", "-x", "devtmpfs", "-x", "overlay", "-x", "squashfs", "-x", "efivarfs", "-x", "nsfs")
	var out []disk
	seen := map[string]bool{}
	for _, l := range strings.Split(b.String(), "\n")[1:] {
		f := strings.Fields(l)
		if len(f) < 6 || seen[f[5]] {
			continue
		}
		total, err1 := strconv.ParseUint(f[1], 10, 64)
		used, err2 := strconv.ParseUint(f[2], 10, 64)
		if err1 != nil || err2 != nil || total == 0 || strings.HasPrefix(f[5], "/boot") {
			continue
		}
		seen[f[5]] = true
		out = append(out, disk{Mount: strings.Join(f[5:], " "), Used: used * 1024, Total: total * 1024})
	}
	return out
}

// proc is a process's CPU time so far, in ticks.
type proc struct {
	PID   int
	Name  string
	Ticks uint64
}

// readProcs reads every process's name and CPU time.
func readProcs() map[int]proc {
	out := map[int]proc{}
	dirs, _ := os.ReadDir(procRoot)
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(procRoot, d.Name(), "stat"))
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
		out[pid] = proc{PID: pid, Name: s[open+1 : end], Ticks: ut + st}
	}
	return out
}

// procUse is a process's share of the host's CPU between two readings.
type procUse struct {
	Name    string
	PID     int
	Percent float64 // of the whole host: 100 is every CPU busy
}

// topProcs ranks the processes by the CPU they took between before and
// after, grouping by name (qbittorrent-nox, dockerd, ...).
func topProcs(before, after map[int]proc, cpu cpuTimes, cpuNext cpuTimes, n int) []procUse {
	dt := float64(cpuNext.total - cpu.total)
	if dt <= 0 {
		return nil
	}
	byName := map[string]*procUse{}
	for pid, p := range after {
		prev, ok := before[pid]
		if !ok || p.Ticks < prev.Ticks {
			continue
		}
		u := byName[p.Name]
		if u == nil {
			u = &procUse{Name: p.Name, PID: pid}
			byName[p.Name] = u
		}
		u.Percent += 100 * float64(p.Ticks-prev.Ticks) / dt
	}
	var out []procUse
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

// containerUse is a reflux container's CPU time and memory.
type containerUse struct {
	Name     string
	CPUUsec  uint64
	MemBytes uint64
}

// containerIDs maps the running reflux containers to their full ids.
func containerIDs() map[string]string {
	var b strings.Builder
	quiet(&b, "ps", "--no-trunc", "--filter", "name=^reflux-", "--format", "{{.Names}} {{.ID}}")
	out := map[string]string{}
	for _, l := range strings.Split(b.String(), "\n") {
		if name, id, ok := strings.Cut(strings.TrimSpace(l), " "); ok {
			out[name] = id
		}
	}
	return out
}

// readContainer reads a container's cgroup (v2, systemd driver).
func readContainer(name, id string) (containerUse, error) {
	dir := filepath.Join(sysRoot, "fs/cgroup/system.slice", "docker-"+id+".scope")
	c := containerUse{Name: name}
	lines, err := readLines(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return c, err
	}
	for _, l := range lines {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "usage_usec" {
			c.CPUUsec, _ = strconv.ParseUint(f[1], 10, 64)
		}
	}
	c.MemBytes, _ = readUint(filepath.Join(dir, "memory.current"))
	return c, nil
}
