package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openflux/transport/ipc"
)

type hostState struct {
	module     bool
	worldUp    bool
	nodeStart  string // RFC 3339; egress started 2026-09-29T17:00:00Z
	cron       string
	dfUsed     string
	docDrops   int
	nodeStatus *ipc.StatusPayload
}

// runDoctor sets up a data directory with one client and fakes the host
// around it as st describes. It returns the doctor's output and error.
func runDoctor(t *testing.T, st hostState) (string, error) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	if err := run([]string{"add", "phone", "--url", testURL, "--no-apply"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	s := Store{Root: home}
	os.WriteFile(filepath.Join(home, "egress", "world-1.conf"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(home, "egress", "ru-direct"), nil, 0o600)

	oldModule, oldCmd := amneziawgModule, runCmd
	t.Cleanup(func() { amneziawgModule, runCmd = oldModule, oldCmd })
	amneziawgModule = filepath.Join(home, "no-module")
	if st.module {
		amneziawgModule = home
	}
	if st.nodeStatus != nil {
		srv := ipc.NewServer(s.ipcPath("phone"), nil)
		if err := srv.Listen(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { srv.Close() })
		go func() {
			for range time.Tick(50 * time.Millisecond) {
				srv.SendStatus(st.nodeStatus)
			}
		}()
	}
	egress := fmt.Sprintf(`{"world":"world-1.conf","world_ok":%t,"world_since":"2026-09-29T19:56:08Z",`+
		`"ru_ok":true,"ru_mode":"direct","ru_prefixes":8652,"ru_list_updated":"2026-09-29T19:50:09Z","killswitch_dropped":0}`, st.worldUp)
	runDocker = func(stdout io.Writer, args ...string) error {
		switch call := strings.Join(args, " "); {
		case strings.HasPrefix(call, "version"):
			io.WriteString(stdout, "28.4.0\n")
		case strings.HasPrefix(call, "compose version"):
			io.WriteString(stdout, "2.39.1\n")
		case strings.HasPrefix(call, "image inspect"):
			io.WriteString(stdout, "9f4bef4845796f9b514216fea5fc1e6a80d0fd33 2026-09-29T19:49:54Z\n")
		case strings.HasPrefix(call, "inspect --format {{.State.Status}}"):
			io.WriteString(stdout, "running healthy\n")
		case strings.HasPrefix(call, "inspect"):
			io.WriteString(stdout, "/reflux-egress true 2026-09-29T17:00:00Z\n/reflux-node-phone true "+st.nodeStart+"\n")
		case call == "exec reflux-egress cat /run/reflux-egress/status.json":
			io.WriteString(stdout, egress)
		case strings.HasPrefix(call, "logs"):
			for i := 0; i < st.docDrops; i++ {
				io.WriteString(dockerStderr, "2026/09/30 11:00:00 [M-DOCS] connection to the document dropped: websocket: close 1005 (no status); reconnecting\n")
			}
		case strings.HasPrefix(call, "info"):
			io.WriteString(stdout, "/var/lib/docker\n")
		}
		return nil
	}
	runCmd = func(stdout io.Writer, name string, args ...string) error {
		switch name {
		case "crontab":
			io.WriteString(stdout, st.cron)
		case "df":
			io.WriteString(stdout, "Filesystem 1024-blocks Used Available Capacity Mounted on\n"+
				"/dev/sdd1 952195668 769784608 133968436 "+st.dfUsed+"% /\n"+
				"/dev/sdd1 952195668 769784608 133968436 "+st.dfUsed+"% /\n")
		}
		return nil
	}
	var out strings.Builder
	err := run([]string{"doctor"}, nil, &out)
	return out.String(), err
}

func TestDoctorOnAHealthyHost(t *testing.T) {
	out, err := runDoctor(t, hostState{
		module: true, worldUp: true, nodeStart: "2026-09-29T17:00:05Z",
		cron: "* * * * * $HOME/.local/bin/reflux heal >> $HOME/reflux/heal.log 2>&1\n", dfUsed: "86",
		nodeStatus: &ipc.StatusPayload{Running: true, Connected: true, UptimeMs: 16 * 3600 * 1000, BytesOut: 1_400_000_000},
	})
	if err != nil || strings.Contains(out, "FAIL  ") || strings.Contains(out, "warn  ") {
		t.Fatalf("err=%v\n%s", err, out)
	}
	for _, want := range []string{
		"ok    amneziawg kernel module loaded",
		"ok    egress configs: world world-1.conf; russia direct",
		"commit 9f4bef4, built",
		"ok    world up via world-1.conf",
		"ok    node phone: up 16h, client online, 1.4 GB down",
		"ok    cron runs reflux heal",
		"ok    disk / 86% used",
		"0 problem(s), 0 warning(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "disk /") != 1 {
		t.Errorf("one filesystem reported twice:\n%s", out)
	}
}

func TestDoctorNamesEachProblem(t *testing.T) {
	out, err := runDoctor(t, hostState{
		module: false, worldUp: false, nodeStart: "2026-09-29T16:59:00Z", // before egress
		cron: "", dfUsed: "98",
	})
	if err == nil {
		t.Fatalf("doctor found nothing:\n%s", out)
	}
	for _, want := range []string{
		"FAIL  amneziawg kernel module not loaded",
		"FAIL  world DOWN",
		"FAIL  node phone started before egress and has no network: reflux heal",
		"warn  cron does not run reflux heal",
		"FAIL  disk / 98% full",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestDoctorNoticesADocumentThatKeepsDropping(t *testing.T) {
	out, _ := runDoctor(t, hostState{
		module: true, worldUp: true, nodeStart: "2026-09-29T17:00:05Z", cron: "reflux heal", dfUsed: "10",
		docDrops: 4, nodeStatus: &ipc.StatusPayload{Running: true},
	})
	if !strings.Contains(out, "warn  node phone lost its document 4 times in 5 minutes") {
		t.Errorf("no warning about the document:\n%s", out)
	}
}

// crontab -l logs to the system log: the bot, checking every minute,
// reads the crontab once an hour.
func TestTheBotReadsTheCrontabHourly(t *testing.T) {
	s := Store{Root: t.TempDir()}
	fakeDocker(t, "")
	n := 0
	old := runCmd
	runCmd = func(stdout io.Writer, name string, args ...string) error {
		if name == "crontab" {
			n++
			io.WriteString(stdout, "* * * * * reflux heal\n")
		}
		return nil
	}
	defer func() { runCmd, cronEvery, cronLast.at = old, 0, time.Time{} }()
	runChecks(s)
	runChecks(s)
	if n != 2 {
		t.Errorf("doctor read the crontab %d times in 2 runs, want every run", n)
	}
	cronEvery = time.Hour
	fs1, fs2 := runChecks(s), runChecks(s)
	if n != 3 {
		t.Errorf("the bot read the crontab %d more times in 2 runs, want 1", n-2)
	}
	if formatFindings(fs1) != formatFindings(fs2) {
		t.Errorf("the reused cron finding differs:\n%s\n%s", formatFindings(fs1), formatFindings(fs2))
	}
}
