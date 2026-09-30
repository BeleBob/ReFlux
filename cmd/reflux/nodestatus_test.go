package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"openflux/transport/ipc"
)

// An update that changes node.conf must reach nodes already running: they
// read it at start only.
func TestApplyRecreatesNodesWithAnOutdatedConf(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	for _, n := range []string{"old", "fresh"} {
		if err := run([]string{"add", n, "--url", testURL + n, "--no-apply"}, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	s := Store{Root: home}
	// "old" runs a config from before IPCSocket.
	oldConf := filepath.Join(s.clientDir("old"), "node.conf")
	b, _ := os.ReadFile(oldConf)
	os.WriteFile(oldConf, []byte(strings.Replace(string(b), "IPCSocket = /state/ipc.sock\n", "", 1)), 0o600)
	started := time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	var calls []string
	runDocker = func(stdout io.Writer, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "inspect" {
			// "old" started before its config changed; "fresh" after.
			io.WriteString(stdout, "/reflux-egress true 2026-09-29T17:00:00Z\n"+
				"/reflux-node-old true 2026-09-29T17:00:05Z\n"+
				"/reflux-node-fresh true "+started+"\n")
		}
		return nil
	}
	if err := run([]string{"apply"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(oldConf); !strings.Contains(string(b), "IPCSocket = /state/ipc.sock\n") {
		t.Errorf("node.conf not brought up to date:\n%s", b)
	}
	last := calls[len(calls)-1]
	if !strings.HasSuffix(last, "up --detach --no-deps --force-recreate node-old") {
		t.Errorf("apply ended with %q, want only node-old recreated", last)
	}
}

func TestSyncConfLeavesACurrentConfAlone(t *testing.T) {
	s := Store{Root: t.TempDir()}
	c, err := s.Add("phone", "mailru", testURL)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.clientDir("phone"), "node.conf")
	past := time.Now().Add(-time.Hour)
	os.Chtimes(path, past, past)
	if err := s.SyncConf(c); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); !st.ModTime().Equal(past) {
		t.Error("an up-to-date node.conf was rewritten (its node would be recreated for nothing)")
	}
}

// The list reads each node's status from the core's own IPC bridge.
func TestListShowsWhoIsOnline(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	for _, n := range []string{"laptop", "phone", "tablet"} {
		if err := run([]string{"add", n, "--url", testURL + n, "--no-apply"}, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	s := Store{Root: home}
	serve := func(name string, st ipc.StatusPayload) {
		srv := ipc.NewServer(s.ipcPath(name), nil)
		if err := srv.Listen(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { srv.Close() })
		go func() {
			for range time.Tick(50 * time.Millisecond) {
				srv.SendStatus(&st)
			}
		}()
	}
	serve("phone", ipc.StatusPayload{Running: true, Connected: true, BytesOut: 1_400_000_000, BytesIn: 52_000_000})
	serve("tablet", ipc.StatusPayload{Running: true})
	// laptop: no socket (stopped, or an image without IPCSocket).

	var out strings.Builder
	if err := run([]string{"list"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	lines := map[string]string{}
	for _, l := range strings.Split(out.String(), "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			lines[f[0]] = strings.Join(f, " ")
		}
	}
	for name, want := range map[string]string{
		"phone":  "online 1.4 GB 52 MB",
		"tablet": "offline 0 B 0 B",
		"laptop": "not running - - -",
	} {
		if !strings.HasSuffix(lines[name], want) {
			t.Errorf("%s: %q, want it to end with %q\n%s", name, lines[name], want, out.String())
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[uint64]string{0: "0 B", 999: "999 B", 1000: "1.0 KB", 812_345: "812 KB",
		1_400_000_000: "1.4 GB", 25_000_000_000: "25 GB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
