package main

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseExpiry(t *testing.T) {
	now := time.Date(2026, 9, 30, 15, 4, 5, 0, time.Local)
	for in, want := range map[string]time.Time{
		"never":      {},
		"30d":        time.Date(2026, 10, 30, 15, 4, 5, 0, time.Local),
		"2w":         time.Date(2026, 10, 14, 15, 4, 5, 0, time.Local),
		"12h":        now.Add(12 * time.Hour),
		"2026-12-31": time.Date(2027, 1, 1, 0, 0, 0, 0, time.Local), // through that day
	} {
		got, err := parseExpiry(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseExpiry(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0d", "-1d", "30", "tomorrow", "-2h", "2026-13-01", "99999d"} {
		if _, err := parseExpiry(bad, now); err == nil {
			t.Errorf("parseExpiry(%q) accepted", bad)
		}
	}
}

func TestInactiveClientsGetNoNode(t *testing.T) {
	now := time.Now()
	clients := []Client{
		{Name: "on"},
		{Name: "paused", Paused: true},
		{Name: "expired", Expires: now.Add(-time.Minute)},
		{Name: "until", Expires: now.Add(time.Hour)},
	}
	var names []string
	for _, c := range activeClients(clients, now) {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "on,until" {
		t.Errorf("active = %v", names)
	}
}

func TestPauseResumeKeepTheKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	calls := fakeDocker(t, "")
	if err := run([]string{"add", "phone", "--url", testURL, "--no-apply"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	s := Store{Root: home}
	key, _ := s.Key("phone")
	if err := run([]string{"pause", "phone"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	c, _ := s.Get("phone")
	if !c.Paused {
		t.Error("not paused")
	}
	compose := readCompose(t, s)
	if strings.Contains(compose, "node-phone") {
		t.Error("a paused client still has a node in compose.yml")
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "up --detach --remove-orphans") {
		t.Errorf("pause did not apply: %q", *calls)
	}
	if err := run([]string{"resume", "phone"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.Key("phone"); again != key {
		t.Error("pause/resume changed the key")
	}
	if !strings.Contains(readCompose(t, s), "node-phone") {
		t.Error("a resumed client has no node")
	}
}

func TestResumeRefusesAnExpiredClient(t *testing.T) {
	t.Setenv("REFLUX_HOME", t.TempDir())
	fakeDocker(t, "")
	if err := run([]string{"add", "phone", "--url", testURL, "--no-apply", "--expires", "1h"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"expire", "phone", "1s"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := run([]string{"resume", "phone"}, nil, io.Discard); err == nil {
		t.Error("resume of an expired client succeeded")
	}
	var out strings.Builder
	if err := run([]string{"list"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), " expired ") {
		t.Errorf("list does not say expired:\n%s", out.String())
	}
	if err := run([]string{"expire", "phone", "never"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"resume", "phone"}, nil, io.Discard); err != nil {
		t.Errorf("resume after extending: %v", err)
	}
}

// Expiry takes effect without anyone running a command: heal, from cron.
func TestHealStopsExpiredNodes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	fakeDocker(t, "")
	for _, n := range []string{"phone", "guest"} {
		if err := run([]string{"add", n, "--url", testURL + n, "--no-apply"}, nil, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	s := Store{Root: home}
	c, _ := s.Get("guest")
	c.Expires = time.Now().Add(-time.Minute)
	if err := s.Save(c); err != nil {
		t.Fatal(err)
	}
	var calls []string
	runDocker = func(stdout io.Writer, args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "inspect" {
			io.WriteString(stdout, "/reflux-egress true 2026-09-29T17:00:00Z\n"+
				"/reflux-node-phone true 2026-09-29T17:00:05Z\n/reflux-node-guest true 2026-09-29T17:00:05Z\n")
		}
		return nil
	}
	var out strings.Builder
	if err := run([]string{"heal"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "access ended for guest") {
		t.Errorf("heal said %q", out.String())
	}
	if strings.Contains(readCompose(t, s), "node-guest") {
		t.Error("the expired node is still in compose.yml")
	}
	if !strings.Contains(strings.Join(calls, "\n"), "up --detach --remove-orphans") {
		t.Errorf("heal did not apply: %q", calls)
	}
}

func readCompose(t *testing.T, s Store) string {
	t.Helper()
	b, err := os.ReadFile(composePath(s))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
