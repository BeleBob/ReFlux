package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/host"
)

func TestMain(m *testing.M) {
	// No test reads this machine's sensors or processes: a hot or busy
	// machine must not change what the checks say. Tests that need them
	// fake them (fakeHostTree).
	host.ProcRoot, host.SysRoot = "/nonexistent/proc", "/nonexistent/sys"
	os.Exit(m.Run())
}

// resetHostWatch forgets the CPU samples before and after a test.
func resetHostWatch(t *testing.T) {
	t.Helper()
	clear := func() {
		hostWatch.Lock()
		hostWatch.prevCPU, hostWatch.prevProcs, hostWatch.prevAt = host.CPUTimes{}, nil, time.Time{}
		hostWatch.measured, hostWatch.percent, hostWatch.top = false, 0, ""
		hostWatch.busySince, hostWatch.hot = time.Time{}, false
		hostWatch.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// heatFindings runs the temperature and CPU check.
func heatFindings(s Store) []finding {
	d := &doctor{}
	d.heat(s)
	return d.findings
}

func TestTemperatureWarning(t *testing.T) {
	resetHostWatch(t)
	_, sys := fakeHostTree(t)
	s := Store{Root: t.TempDir()}
	hw := sys + "/class/hwmon/hwmon1/"
	writeFile(t, hw+"name", "coretemp\n")
	writeFile(t, hw+"temp1_label", "Package id 0\n")
	writeFile(t, hw+"temp1_max", "82000\n")
	writeFile(t, hw+"temp1_crit", "102000\n")
	for _, c := range []struct {
		c    string
		want string
	}{
		{"41000", "temp.ok"},
		{"83000", "temp.hot"},  // the sensor's own limit, 82 °C
		{"79000", "temp.hot"},  // stays on until 5 °C under it
		{"76000", "temp.ok"},   // ...here
		{"98000", "temp.crit"}, // 5 °C under the sensor's critical point
	} {
		writeFile(t, hw+"temp1_input", c.c+"\n")
		fs := heatFindings(s)
		if len(fs) != 1 || fs[0].Msg != c.want || fs[0].Key != "temp" || category(fs[0].Key) != "server" {
			t.Errorf("%s m°C: %+v, want %s", c.c, fs, c.want)
		}
	}
	if fs := heatFindings(s); fs[0].text(langRU) != "температура процессора 98 °C — у предела (97 °C): процессор замедляется и может выключиться" {
		t.Errorf("text %q", fs[0].text(langRU))
	}
	// The owner's threshold, from the bot's settings.
	resetHostWatch(t)
	s.saveBotConfig(botConfig{Token: testToken, Chat: 42, TempWarn: 70})
	writeFile(t, hw+"temp1_input", "72000\n")
	if fs := heatFindings(s); fs[0].Msg != "temp.hot" || !strings.Contains(fs[0].text(langEN), "starts at 70 °C") {
		t.Errorf("owner's 70 °C: %+v", fs)
	}
	// A sensor without limits: 80 and 95 °C.
	resetHostWatch(t)
	s.saveBotConfig(botConfig{Token: testToken, Chat: 42})
	os.Remove(hw + "temp1_max")
	os.Remove(hw + "temp1_crit")
	for v, want := range map[string]string{"79000": "temp.ok", "81000": "temp.hot", "95000": "temp.crit"} {
		resetHostWatch(t)
		writeFile(t, hw+"temp1_input", v+"\n")
		if fs := heatFindings(s); fs[0].Msg != want {
			t.Errorf("no limits, %s: %+v, want %s", v, fs, want)
		}
	}
	// No sensors: nothing to say.
	fakeHostTree(t)
	if fs := heatFindings(s); len(fs) != 0 {
		t.Errorf("no sensors: %+v", fs)
	}
}

func TestCPUBusyWarning(t *testing.T) {
	resetHostWatch(t)
	proc, _ := fakeHostTree(t)
	s := Store{Root: t.TempDir()}
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	busy, total, qbt := 0, 0, 0
	// minute advances the fake host a minute at pct percent busy, most of
	// it qbittorrent-nox's.
	minute := func(i, pct int) {
		busy += 60 * pct
		total += 6000
		qbt += 50 * pct
		writeFile(t, proc+"/stat", "cpu  "+itoa(busy)+" 0 0 "+itoa(total-busy)+" 0 0 0 0 0 0\n")
		writeFile(t, proc+"/77/stat", "77 (qbittorrent-nox) S 1 1 1 0 -1 0 0 0 0 0 "+itoa(qbt)+" 0 0 0 20 0 1 0\n")
		sampleCPU(start.Add(time.Duration(i)*time.Minute), defaultCPUWarn)
	}
	minute(0, 0)
	if fs := heatFindings(s); len(fs) != 0 {
		t.Errorf("one sample, no interval yet: %+v", fs)
	}
	for i := 1; i <= 10; i++ {
		minute(i, 95)
		fs := heatFindings(s)
		want := "cpu.ok"
		if i == 10 {
			want = "cpu.busy"
		}
		if len(fs) != 1 || fs[0].Msg != want || category(fs[0].Key) != "server" {
			t.Fatalf("minute %d: %+v, want %s", i, fs, want)
		}
	}
	if got := heatFindings(s)[0].text(langEN); got != "CPU 95% busy for 10m, mostly qbittorrent-nox: the channels slow down" {
		t.Errorf("text %q", got)
	}
	minute(11, 40)
	if fs := heatFindings(s); fs[0].Msg != "cpu.ok" || shortText(s, langRU, fs[0]) != "Процессор: 40%" {
		t.Errorf("after a quiet minute: %+v", fs)
	}
	// The owner's threshold: 80%.
	s.saveBotConfig(botConfig{Token: testToken, Chat: 42, CPUWarn: 80})
	for i := 12; i <= 22; i++ {
		busy += 60 * 85
		total += 6000
		writeFile(t, proc+"/stat", "cpu  "+itoa(busy)+" 0 0 "+itoa(total-busy)+" 0 0 0 0 0 0\n")
		sampleCPU(start.Add(time.Duration(i)*time.Minute), s.hostLimits().CPUWarn)
	}
	if fs := heatFindings(s); fs[0].Msg != "cpu.busy" {
		t.Errorf("85%% for 10 minutes over the owner's 80%%: %+v", fs)
	}
}

func TestLimitSettings(t *testing.T) {
	resetHostWatch(t)
	_, sys := fakeHostTree(t)
	writeFile(t, sys+"/class/hwmon/hwmon1/name", "coretemp\n")
	writeFile(t, sys+"/class/hwmon/hwmon1/temp1_input", "40000\n")
	writeFile(t, sys+"/class/hwmon/hwmon1/temp1_max", "82000\n")
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	s := Store{Root: home}
	s.saveBotConfig(botConfig{Token: testToken, Chat: 42})
	f := newFakeTG(t)
	c, _ := s.loadBotConfig()
	b := newBot(s, c)
	b.handle(press(1, 42, "set", time.Now()))
	if got := f.lastEdit(); !strings.Contains(got, "температура от 82 °C (по датчику); процессор занят на 90% и больше 10 минут") {
		t.Errorf("settings:\n%s", got)
	}
	b.handle(press(2, 42, "lim:t-", time.Now())) // from the sensor's 82: 80, then 75
	b.handle(press(3, 42, "lim:t-", time.Now()))
	for i := 0; i < 6; i++ {
		b.handle(press(int64(4+i), 42, "lim:c+", time.Now())) // up to 100 and no further
	}
	if c, _ := s.loadBotConfig(); c.TempWarn != 75 || c.CPUWarn != 100 || c.Token != testToken {
		t.Errorf("saved %+v", c)
	}
	for i := 0; i < 10; i++ {
		b.handle(press(int64(20+i), 42, "lim:t-", time.Now()))
	}
	if c, _ := s.loadBotConfig(); c.TempWarn != 60 {
		t.Errorf("temperature down to %d, want 60 at least", c.TempWarn)
	}
	if got := f.lastEdit(); !strings.Contains(got, "температура от 60 °C;") {
		t.Errorf("settings after:\n%s", got)
	}
}

func (f *fakeTG) lastEdit() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.edited) == 0 {
		return ""
	}
	return f.edited[len(f.edited)-1]
}
