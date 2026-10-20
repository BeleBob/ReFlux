package main

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientLogsCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	if err := run([]string{"add", "phone", "--url", testURL, "--no-apply"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	s := Store{Root: home}
	if conf := nodeConf(mustGet(t, s, "phone")); !strings.Contains(conf, "ClientLogDir = /state/client-logs\n") {
		t.Errorf("node.conf has no ClientLogDir:\n%s", conf)
	}
	var out strings.Builder
	if err := run([]string{"client-logs"}, nil, &out); err != nil || !strings.Contains(out.String(), "No client has sent a log") {
		t.Fatalf("no logs yet: %q, %v", out.String(), err)
	}
	dir := s.clientLogDir("phone")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "client-log-20261020T120000Z.txt"), []byte("the first\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "client-log-20261020T130000Z.txt"), []byte("the second\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "sent-20261020T110000Z"), nil, 0o600)
	os.WriteFile(filepath.Join(home, "secret.txt"), []byte("not a log\n"), 0o600)

	out.Reset()
	if err := run([]string{"client-logs"}, nil, &out); err != nil || !strings.Contains(out.String(), "phone") || !strings.Contains(out.String(), " 2 log(s)") {
		t.Errorf("who sent logs: %q, %v", out.String(), err)
	}
	out.Reset()
	if err := run([]string{"client-logs", "phone"}, nil, &out); err != nil || strings.Count(out.String(), "client-log-") != 2 {
		t.Errorf("phone's logs: %q, %v", out.String(), err)
	}
	out.Reset()
	if err := run([]string{"client-logs", "phone", "last"}, nil, &out); err != nil || out.String() != "the second\n" {
		t.Errorf("the last log: %q, %v", out.String(), err)
	}
	out.Reset()
	if err := run([]string{"client-logs", "phone", "client-log-20261020T120000Z.txt"}, nil, &out); err != nil || out.String() != "the first\n" {
		t.Errorf("a log by name: %q, %v", out.String(), err)
	}
	for _, bad := range []string{"../../secret.txt", filepath.Join(home, "secret.txt"), "sent-20261020T110000Z"} {
		out.Reset()
		if err := run([]string{"client-logs", "phone", bad}, nil, &out); err == nil || strings.Contains(out.String(), "not a log") {
			t.Errorf("printed %q for %q", out.String(), bad)
		}
	}
	for rel, want := range map[string]bool{
		"state/phone/client-logs":                               true,
		"state/phone/client-logs/client-log-20261020T1200Z.txt": true,
		"state/phone/traffic.json":                              false,
	} {
		mode := fs.FileMode(0o600)
		if !strings.HasSuffix(rel, ".txt") && !strings.HasSuffix(rel, ".json") {
			mode = fs.ModeDir | 0o700
		}
		if backupSkip(rel, mode) != want {
			t.Errorf("backupSkip(%q) != %v", rel, want)
		}
	}
}

func mustGet(t *testing.T, s Store, name string) Client {
	t.Helper()
	c, err := s.Get(name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
