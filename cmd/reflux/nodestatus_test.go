package main

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/ipc"
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

// The core serves one IPC client at a time and a new one cuts the last:
// a reading cut by another reader (the panel and the bot at once) is
// asked again, so the node does not look silent.
func TestStatusSurvivesAnotherReader(t *testing.T) {
	s := Store{Root: t.TempDir()}
	os.MkdirAll(s.stateDir("phone"), 0o700)
	srv := ipc.NewServer(s.ipcPath("phone"), nil)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	st := ipc.StatusPayload{Running: true, Connected: true}
	go func() {
		for range time.Tick(250 * time.Millisecond) {
			srv.SendStatus(&st)
		}
	}()
	old := statusRetry
	statusRetry = 50 * time.Millisecond
	t.Cleanup(func() { statusRetry = old })
	// Another reader arrives just after ours, and cuts it off.
	go func() {
		time.Sleep(30 * time.Millisecond)
		if c, err := readNodeStatusConn(s.ipcPath("phone")); err == nil {
			time.Sleep(20 * time.Millisecond)
			c.Close()
		}
	}()
	got := nodeStatuses(s, []Client{{Name: "phone"}})
	if p, ok := got["phone"]; !ok || !p.Connected {
		t.Errorf("status %+v (%v): a cut reading was not asked again", p, ok)
	}
}

// readNodeStatusConn connects to a node's IPC socket like another reader.
func readNodeStatusConn(path string) (net.Conn, error) { return net.Dial("unix", path) }

// A node that gives no status is shown, but never alerted: a lost reading
// would come back as "the node works again".
func TestNoStatusIsNotAnAlert(t *testing.T) {
	m := monitor{confirm: 1}
	ok := finding{Key: "node:phone", Level: levelOK, Msg: "node.ok", Sig: levelOK.String()}
	d := &doctor{}
	d.add(levelWarn, "node:phone", levelOK.String(), "node.nostatus", "phone")
	quiet := d.findings[0]
	m.update([]finding{ok}, langRU)
	for _, fs := range [][]finding{{quiet}, {ok}, {quiet}, {ok}} {
		if news := m.update(fs, langRU); len(news) != 0 {
			t.Errorf("alerts %+v", news)
		}
	}
}

// resetSeen forgets what this process saw, before and after a test.
func resetSeen(t *testing.T) {
	t.Helper()
	clear := func() {
		seenCache.Lock()
		seenCache.m, seenCache.savedAt = map[string]time.Time{}, time.Time{}
		seenCache.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

func TestLastSeen(t *testing.T) {
	resetSeen(t)
	s := Store{Root: t.TempDir()}
	t0 := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	s.markSeen([]string{"phone"}, t0)
	if got := s.readSeen()["phone"]; !got.Equal(t0) {
		t.Fatalf("first mark not written: %v", got)
	}
	// Within a minute: remembered, not written.
	s.markSeen([]string{"phone"}, t0.Add(10*time.Second))
	if got := s.readSeen()["phone"]; !got.Equal(t0) {
		t.Errorf("written within a minute: %v", got)
	}
	if got := s.lastSeen()["phone"]; !got.Equal(t0.Add(10 * time.Second)) {
		t.Errorf("last seen %v", got)
	}
	// Another process wrote a later time for another client: both stay.
	m := s.readSeen()
	m["tablet"] = t0.Add(30 * time.Second)
	writeJSON(s.seenPath(), m)
	s.markSeen([]string{"phone"}, t0.Add(2*time.Minute))
	got := s.readSeen()
	if !got["phone"].Equal(t0.Add(2*time.Minute)) || !got["tablet"].Equal(t0.Add(30*time.Second)) {
		t.Errorf("merged %v", got)
	}
}

// Reading a node marks its connected client, and the pages say when an
// offline one was last there.
func TestLastSeenShown(t *testing.T) {
	resetSeen(t)
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	s := Store{Root: home}
	run([]string{"add", "phone", "--url", testURL, "--no-apply"}, nil, io.Discard)
	srv := ipc.NewServer(s.ipcPath("phone"), nil)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	st := ipc.StatusPayload{Running: true, Connected: true}
	go func() {
		for range time.Tick(50 * time.Millisecond) {
			srv.SendStatus(&st)
		}
	}()
	c, _ := s.Get("phone")
	nodeStatuses(s, []Client{c})
	if s.readSeen()["phone"].IsZero() {
		t.Fatal("a connected client not marked")
	}
	// Two hours later, offline.
	before := time.Now().Add(-2 * time.Hour)
	seenCache.Lock()
	seenCache.m["phone"] = before
	seenCache.Unlock()
	writeJSON(s.seenPath(), map[string]time.Time{"phone": before})
	v := clientView{c: c, active: true, running: true, status: &statusView{}, seen: s.lastSeen()["phone"]}
	if got := stateText(langRU, v); !strings.HasPrefix(got, "не в сети, был 2 ч назад") {
		t.Errorf("state %q", got)
	}
	v.seen = time.Time{}
	if got := stateText(langRU, v); !strings.HasPrefix(got, "не в сети ·") {
		t.Errorf("never seen: %q", got)
	}
}
