package main

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunning makes docker report the given containers running since the
// given times, and their logs say which run they belong to.
func fakeRunning(t *testing.T, started map[string]string) *[]string {
	t.Helper()
	var calls []string
	oldDocker, oldLogs, oldRules := runDocker, dockerLogs, readRules
	runDocker = func(stdout io.Writer, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "inspect" {
			for _, name := range args[3:] {
				if at, ok := started[name]; ok {
					io.WriteString(stdout, "/"+name+" true "+at+"\n")
				}
			}
		}
		return nil
	}
	dockerLogs = func(name string, w io.Writer) error {
		calls = append(calls, "logs "+name)
		_, err := io.WriteString(w, "log of "+name+" since "+started[name]+"\n")
		return err
	}
	readRules = func() (string, error) { return plainHostRules, nil }
	t.Cleanup(func() { runDocker, dockerLogs, readRules = oldDocker, oldLogs, oldRules })
	return &calls
}

func logFiles(t *testing.T, s Store) []string {
	t.Helper()
	entries, err := os.ReadDir(s.logsDir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// A node's log outlives the node: restarting it keeps the run's log first.
func TestRestartKeepsTheNodesLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	if err := run([]string{"add", "phone", "--url", testURL, "--no-apply"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	calls := fakeRunning(t, map[string]string{"reflux-node-phone": "2026-10-19T12:31:01.5Z"})
	if err := run([]string{"restart", "phone"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*calls, "\n")
	logs, up := strings.Index(joined, "logs reflux-node-phone"), strings.Index(joined, "--force-recreate node-phone")
	if logs < 0 || up < 0 || logs > up {
		t.Fatalf("want the log kept before the node is recreated; docker calls:\n%s", joined)
	}
	s := Store{Root: home}
	b, err := os.ReadFile(filepath.Join(s.logsDir(), "reflux-node-phone-20261019T123101Z.log"))
	if err != nil || !strings.Contains(string(b), "log of reflux-node-phone") {
		t.Fatalf("kept log %q, %v; logs/ has %v", b, err, logFiles(t, s))
	}
	if fi, err := os.Stat(s.logsDir()); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("logs/ mode %v, %v; want 0700 (logs name the documents)", fi.Mode().Perm(), err)
	}
}

// One file per run of a container: saving the same run again refreshes it,
// and only the last keptContainerLogs runs stay. Node "a" leaves the logs
// of node "a-b" alone.
func TestKeptLogsOnePerRun(t *testing.T) {
	s := Store{Root: t.TempDir()}
	started := map[string]string{"reflux-node-a-b": "2026-10-01T00:00:00Z"}
	fakeRunning(t, started)
	saveLogs(s, []string{"reflux-node-a-b"}, io.Discard)
	for _, at := range []string{"2026-10-01T00:00:00Z", "2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z", "2026-10-03T00:00:00Z", "2026-10-04T00:00:00Z"} {
		started["reflux-node-a"] = at
		saveLogs(s, []string{"reflux-node-a", "reflux-node-gone"}, io.Discard)
	}
	got := strings.Join(logFiles(t, s), " ")
	want := "reflux-node-a-20261002T000000Z.log reflux-node-a-20261003T000000Z.log reflux-node-a-20261004T000000Z.log reflux-node-a-b-20261001T000000Z.log"
	if got != want {
		t.Errorf("logs/:\n got %s\nwant %s", got, want)
	}
}

// Backups leave the kept logs out, and a restore leaves them in place.
func TestBackupSkipsKeptLogs(t *testing.T) {
	for rel, want := range map[string]bool{"logs": true, "logs/reflux-egress-20261019T123101Z.log": true, "logsheet.json": false, "clients/logs/client.json": false} {
		mode := fs.FileMode(0o600)
		if !strings.HasSuffix(rel, ".json") && !strings.HasSuffix(rel, ".log") {
			mode = fs.ModeDir | 0o700
		}
		if got := backupSkip(rel, mode); got != want {
			t.Errorf("backupSkip(%q) = %v, want %v", rel, got, want)
		}
	}
}
