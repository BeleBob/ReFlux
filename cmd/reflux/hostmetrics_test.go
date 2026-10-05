package main

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/ipc"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/host"
)

// fakeHostTree points host.ProcRoot and host.SysRoot at a temporary tree.
func fakeHostTree(t *testing.T) (proc, sys string) {
	t.Helper()
	root := t.TempDir()
	proc, sys = filepath.Join(root, "proc"), filepath.Join(root, "sys")
	oldP, oldS := host.ProcRoot, host.SysRoot
	host.ProcRoot, host.SysRoot = proc, sys
	t.Cleanup(func() { host.ProcRoot, host.SysRoot = oldP, oldS })
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
	if lines, _ := host.ReadLines(s.eventsPath()); len(lines) > eventsMax {
		t.Errorf("log not trimmed: %d lines", len(lines))
	}
}
