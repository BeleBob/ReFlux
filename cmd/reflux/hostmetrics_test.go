package main

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/ipc"
)

// fakeHostTree points procRoot and sysRoot at a temporary tree.
func fakeHostTree(t *testing.T) (proc, sys string) {
	t.Helper()
	root := t.TempDir()
	proc, sys = filepath.Join(root, "proc"), filepath.Join(root, "sys")
	oldP, oldS := procRoot, sysRoot
	procRoot, sysRoot = proc, sys
	t.Cleanup(func() { procRoot, sysRoot = oldP, oldS })
	return proc, sys
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHostMetrics(t *testing.T) {
	proc, sys := fakeHostTree(t)
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

	a, err := readCPU()
	if err != nil || a.total != 1000 || a.busy != 200 {
		t.Fatalf("cpu %+v %v", a, err)
	}
	writeFile(t, proc+"/stat", "cpu  400 0 100 800 100 0 0 0 0 0\n")
	b, _ := readCPU()
	if p := a.percent(b); p < 74.9 || p > 75.1 { // 300 busy of 400
		t.Errorf("cpu percent %.1f, want 75", p)
	}
	if m, err := readMemory(); err != nil || m.Used() != 2000000*1024 || int(m.Percent()) != 25 {
		t.Errorf("memory %+v %v", m, err)
	}
	if l, err := readLoad(); err != nil || l != [3]float64{1.45, 1.12, 1.09} {
		t.Errorf("load %v %v", l, err)
	}
	if u, _ := readUptime(); u != 3600*time.Second+500*time.Millisecond {
		t.Errorf("uptime %v", u)
	}
	temps := readTemps()
	if len(temps) != 2 || temps[0].Label != "Package id 0" || temps[0].C != 41 {
		t.Errorf("temps %+v (the CPU package first, no ACPI zone)", temps)
	}
	if rx, tx, err := ifaceBytes("enp1s0"); rx != 1000 || tx != 2000 || err != nil {
		t.Errorf("iface %d %d %v", rx, tx, err)
	}
	// The bridge transmits what goes into the egress container.
	writeFile(t, sys+"/class/net/"+egressBridge+"/statistics/rx_bytes", "300\n")
	writeFile(t, sys+"/class/net/"+egressBridge+"/statistics/tx_bytes", "7000\n")
	if rx, tx, err := egressBytes(); rx != 7000 || tx != 300 || err != nil {
		t.Errorf("egress received %d, sent %d, %v", rx, tx, err)
	}
}

func TestTopProcsGroupsByName(t *testing.T) {
	proc, _ := fakeHostTree(t)
	stat := func(pid, name string, ut, st int) {
		writeFile(t, proc+"/"+pid+"/stat", pid+" ("+name+") S 1 1 1 0 -1 0 0 0 0 0 "+
			itoa(ut)+" "+itoa(st)+" 0 0 20 0 1 0\n")
	}
	stat("10", "qbittorrent-nox", 100, 50)
	stat("11", "a (weird) name", 0, 0)
	stat("12", "reflux", 10, 0)
	stat("13", "reflux", 10, 0)
	before := readProcs()
	if before[11].Name != "a (weird) name" {
		t.Fatalf("name with parentheses: %+v", before[11])
	}
	stat("10", "qbittorrent-nox", 300, 100) // +250
	stat("12", "reflux", 60, 0)             // +50
	stat("13", "reflux", 40, 0)             // +30
	after := readProcs()
	top := topProcs(before, after, cpuTimes{total: 0}, cpuTimes{total: 1000}, 5)
	if len(top) != 2 || top[0].Name != "qbittorrent-nox" || top[0].Percent != 25 || top[1].Name != "reflux" || top[1].Percent != 8 {
		t.Errorf("top %+v", top)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// A node counts from its start: the sampler adds what it counted since
// the last reading and survives the node's restart and its own.
func TestTrafficSurvivesRestarts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	s := Store{Root: home}
	if _, err := s.Add("phone", "mailru", testURL); err != nil {
		t.Fatal(err)
	}
	srv := ipc.NewServer(s.ipcPath("phone"), nil)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	var mu sync.Mutex
	st := ipc.StatusPayload{BytesOut: 1000, BytesIn: 100, UptimeMs: 10_000}
	go func() {
		for range time.Tick(20 * time.Millisecond) {
			mu.Lock()
			p := st
			mu.Unlock()
			srv.SendStatus(&p)
		}
	}()
	set := func(down, up uint64, uptime int64) {
		mu.Lock()
		st = ipc.StatusPayload{BytesOut: down, BytesIn: up, UptimeMs: uptime}
		mu.Unlock()
		time.Sleep(60 * time.Millisecond)
	}
	day := time.Date(2026, 9, 15, 12, 0, 0, 0, time.Local)
	m := newSampler(s, &sync.Mutex{})
	m.sampleNodes(day)
	set(5000, 300, 20_000)
	m.sampleNodes(day.Add(10 * time.Second))
	set(700, 50, 1_000) // the node restarted
	m.sampleNodes(day.Add(20 * time.Second))
	m.save(true)
	if ct := m.clientTraffic("phone", day); ct.Today.Down != 5700 || ct.Today.Up != 350 {
		t.Fatalf("today %+v, want 5700 down / 350 up", ct.Today)
	}
	// The panel restarts: it reads the file and goes on from there.
	m2 := newSampler(s, &sync.Mutex{})
	set(900, 60, 2_000)
	m2.sampleNodes(day.Add(30 * time.Second))
	ct := m2.clientTraffic("phone", day.Add(24*time.Hour))
	if ct.All.Down != 5900 || ct.All.Up != 360 || ct.Today.Down != 0 || ct.Month.Down != 5900 {
		t.Errorf("after the panel's restart: %+v", ct)
	}
	if len(m.clientTraffic("phone", day).Rates) != 2 {
		t.Errorf("rates %+v", m.clientTraffic("phone", day).Rates)
	}
}

func TestEventLog(t *testing.T) {
	s := Store{Root: t.TempDir()}
	for i := 0; i < eventsMax+5; i += 5 {
		var evs []event
		for j := 0; j < 5; j++ {
			evs = append(evs, event{At: time.Unix(int64(i+j), 0), Level: "info", RU: "событие", EN: "event"})
		}
		if err := s.logEvents(evs); err != nil {
			t.Fatal(err)
		}
	}
	got := s.readEvents(3)
	if len(got) != 3 || got[0].At.Unix() != eventsMax+4 || got[0].Text(langRU) != "событие" || got[0].Text(langEN) != "event" {
		t.Errorf("newest events %+v", got)
	}
	if lines, _ := readLines(s.eventsPath()); len(lines) > eventsMax {
		t.Errorf("log not trimmed: %d lines", len(lines))
	}
}
