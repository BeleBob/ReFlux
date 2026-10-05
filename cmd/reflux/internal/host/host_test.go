package host

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// fakeTree points ProcRoot and SysRoot at a temporary tree.
func fakeTree(t *testing.T) (proc, sys string) {
	t.Helper()
	root := t.TempDir()
	proc, sys = filepath.Join(root, "proc"), filepath.Join(root, "sys")
	oldP, oldS := ProcRoot, SysRoot
	ProcRoot, SysRoot = proc, sys
	t.Cleanup(func() { ProcRoot, SysRoot = oldP, oldS })
	return proc, sys
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestHostMetrics(t *testing.T) {
	proc, sys := fakeTree(t)
	writeFile(t, proc+"/stat", "cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 50 0 50 350 50 0 0 0 0 0\ncpu1 50 0 50 350 50 0 0 0 0 0\nintr 1\n")
	writeFile(t, proc+"/meminfo", "MemTotal:       8000000 kB\nMemFree:  100 kB\nMemAvailable:   6000000 kB\nSwapTotal: 1000 kB\nSwapFree: 500 kB\n")
	writeFile(t, proc+"/loadavg", "1.45 1.12 1.09 1/310 393663\n")
	writeFile(t, proc+"/uptime", "3600.50 7000.00\n")
	writeFile(t, sys+"/class/hwmon/hwmon0/name", "acpitz\n")
	writeFile(t, sys+"/class/hwmon/hwmon0/temp1_input", "27800\n")
	writeFile(t, sys+"/class/hwmon/hwmon1/name", "coretemp\n")
	writeFile(t, sys+"/class/hwmon/hwmon1/temp2_input", "39000\n")
	writeFile(t, sys+"/class/hwmon/hwmon1/temp2_label", "Core 0\n")
	writeFile(t, sys+"/class/hwmon/hwmon1/temp1_input", "41000\n")
	writeFile(t, sys+"/class/hwmon/hwmon1/temp1_label", "Package id 0\n")
	writeFile(t, sys+"/class/net/enp1s0/statistics/rx_bytes", "1000\n")
	writeFile(t, sys+"/class/net/enp1s0/statistics/tx_bytes", "2000\n")

	a, err := ReadCPU()
	if err != nil || a.total != 1000 || a.busy != 200 {
		t.Fatalf("cpu %+v %v", a, err)
	}
	writeFile(t, proc+"/stat", "cpu  400 0 100 800 100 0 0 0 0 0\n")
	b, _ := ReadCPU()
	if p := a.Percent(b); p < 74.9 || p > 75.1 { // 300 busy of 400
		t.Errorf("cpu percent %.1f, want 75", p)
	}
	if m, err := ReadMemory(); err != nil || m.Used() != 2000000*1024 || int(m.Percent()) != 25 {
		t.Errorf("memory %+v %v", m, err)
	}
	if l, err := ReadLoad(); err != nil || l != [3]float64{1.45, 1.12, 1.09} {
		t.Errorf("load %v %v", l, err)
	}
	if u, _ := ReadUptime(); u != 3600*time.Second+500*time.Millisecond {
		t.Errorf("uptime %v", u)
	}
	temps := ReadTemps()
	if len(temps) != 2 || temps[0].Label != "Package id 0" || temps[0].C != 41 {
		t.Errorf("temps %+v (the CPU package first, no ACPI zone)", temps)
	}
	if rx, tx, err := IfaceBytes("enp1s0"); rx != 1000 || tx != 2000 || err != nil {
		t.Errorf("iface %d %d %v", rx, tx, err)
	}
	// The bridge transmits what goes into the egress container.
	writeFile(t, sys+"/class/net/"+EgressBridge+"/statistics/rx_bytes", "300\n")
	writeFile(t, sys+"/class/net/"+EgressBridge+"/statistics/tx_bytes", "7000\n")
	if rx, tx, err := EgressBytes(); rx != 7000 || tx != 300 || err != nil {
		t.Errorf("egress received %d, sent %d, %v", rx, tx, err)
	}
}

func TestTopProcsGroupsByName(t *testing.T) {
	proc, _ := fakeTree(t)
	stat := func(pid, name string, ut, st int) {
		writeFile(t, proc+"/"+pid+"/stat", pid+" ("+name+") S 1 1 1 0 -1 0 0 0 0 0 "+
			itoa(ut)+" "+itoa(st)+" 0 0 20 0 1 0\n")
	}
	stat("10", "qbittorrent-nox", 100, 50)
	stat("11", "a (weird) name", 0, 0)
	stat("12", "reflux", 10, 0)
	stat("13", "reflux", 10, 0)
	before := ReadProcs()
	if before[11].Name != "a (weird) name" {
		t.Fatalf("name with parentheses: %+v", before[11])
	}
	stat("10", "qbittorrent-nox", 300, 100) // +250
	stat("12", "reflux", 60, 0)             // +50
	stat("13", "reflux", 40, 0)             // +30
	after := ReadProcs()
	top := TopProcs(before, after, CPUTimes{total: 0}, CPUTimes{total: 1000}, 5)
	if len(top) != 2 || top[0].Name != "qbittorrent-nox" || top[0].Percent != 25 || top[1].Name != "reflux" || top[1].Percent != 8 {
		t.Errorf("top %+v", top)
	}
}
