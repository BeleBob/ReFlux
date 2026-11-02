package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// deploy/reflux/image-changed.sh on a small module laid out like this one:
// the node is the root package, the egress cmd/reflux-egress, and both use
// a shared package; the app's own code is elsewhere.
func TestImageChangedScript(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the script runs on the CI's Linux")
	}
	for _, tool := range []string{"sh", "git", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	script, err := filepath.Abs("../../deploy/reflux/image-changed.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	run := func(name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(path, body string) {
		t.Helper()
		p := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func() string {
		t.Helper()
		run("git", "add", "-A")
		run("git", "commit", "-q", "--allow-empty", "-m", "c")
		return run("git", "rev-parse", "HEAD")
	}

	write("go.mod", "module example.com/m\n\ngo 1.21\n")
	write("main.go", "package main\n\nimport _ \"example.com/m/transport\"\n\nfunc main() {}\n")
	write("transport/t.go", "package transport\n")
	write("transport/avito/a.go", "package avito\n")
	write("cmd/reflux-egress/main.go", "package main\n\nimport _ \"example.com/m/transport\"\n\nfunc main() {}\n")
	write("cmd/reflux/main.go", "package main\n\nfunc main() {}\n")
	write("mobile/m.go", "package mobile\n")
	write("Dockerfile", "FROM scratch\n")
	write("deploy/reflux/egress/Dockerfile", "FROM scratch\n")
	run("git", "init", "-q")
	base := commit()

	cases := []struct {
		name         string
		change       func()
		node, egress string
	}{
		{"the app only", func() { write("mobile/m.go", "package mobile\n\nvar X = 1\n") }, "false", "false"},
		{"the host CLI only", func() { write("cmd/reflux/main.go", "package main\n\nfunc main() { println() }\n") }, "false", "false"},
		{"a test only", func() { write("transport/t_test.go", "package transport\n") }, "false", "false"},
		{"docs only", func() { write("README.md", "x\n") }, "false", "false"},
		{"a shared package", func() { write("transport/t.go", "package transport\n\nvar X = 1\n") }, "true", "true"},
		{"a package the node does not import", func() { write("transport/avito/a.go", "package avito\n\nvar X = 1\n") }, "false", "false"},
		{"the egress Dockerfile", func() { write("deploy/reflux/egress/Dockerfile", "FROM scratch\nLABEL x=1\n") }, "false", "true"},
		{"the node Dockerfile", func() { write("Dockerfile", "FROM scratch\nLABEL x=1\n") }, "true", "false"},
		{"go.mod", func() { write("go.mod", "module example.com/m\n\ngo 1.22\n") }, "true", "true"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run("git", "reset", "-q", "--hard", base)
			c.change()
			after := commit()
			if got := run("sh", script, "node", base, after); got != c.node {
				t.Errorf("node = %s, want %s", got, c.node)
			}
			if got := run("sh", script, "egress", base, after); got != c.egress {
				t.Errorf("egress = %s, want %s", got, c.egress)
			}
		})
	}
	// No published image (or a commit the clone does not have): build.
	if got := run("sh", script, "node", "none", base); got != "true" {
		t.Errorf("without a base: %s", got)
	}
}
