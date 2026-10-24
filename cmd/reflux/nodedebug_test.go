package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reflux debug writes the level into client.json, renders it into
// node.conf and recreates the node; apply keeps it.
func TestDebugLevelSurvivesApply(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REFLUX_HOME", home)
	calls := fakeDocker(t, "")
	if err := run([]string{"add", "phone", "--url", testURL, "--no-apply"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	s := Store{Root: home}
	conf := filepath.Join(s.clientDir("phone"), "node.conf")
	read := func() string {
		b, err := os.ReadFile(conf)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if strings.Contains(read(), "Debug") {
		t.Error("a new node logs more than its status lines")
	}
	var out strings.Builder
	if err := run([]string{"debug"}, nil, &out); err != nil || !strings.Contains(out.String(), "No node logs") {
		t.Errorf("nothing on: %q, %v", out.String(), err)
	}

	out.Reset()
	if err := run([]string{"debug", "phone", "1"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "per-lane") || !strings.Contains(out.String(), "reflux logs phone") {
		t.Errorf("debug on said %q", out.String())
	}
	if !strings.Contains(read(), "\nDebug = 1\n") {
		t.Errorf("node.conf has no level:\n%s", read())
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "--force-recreate node-phone") {
		t.Errorf("the node was not recreated: %q", *calls)
	}
	if c, _ := s.Get("phone"); c.Debug != 1 {
		t.Errorf("client.json keeps %d", c.Debug)
	}

	// apply and update rewrite node.conf: the level must stay.
	if err := run([]string{"apply"}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(), "\nDebug = 1\n") {
		t.Error("apply dropped the level")
	}
	out.Reset()
	if err := run([]string{"debug"}, nil, &out); err != nil || !strings.Contains(out.String(), "phone") {
		t.Errorf("listing: %q, %v", out.String(), err)
	}

	out.Reset()
	if err := run([]string{"debug", "phone", "0"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(), "Debug") {
		t.Errorf("turning it off left it in node.conf:\n%s", read())
	}
	for _, bad := range []string{"4", "-1", "yes"} {
		if err := run([]string{"debug", "phone", bad}, nil, io.Discard); err == nil {
			t.Errorf("accepted level %q", bad)
		}
	}
	if err := run([]string{"debug", "nobody"}, nil, io.Discard); err == nil {
		t.Error("accepted an unknown client")
	}
}
