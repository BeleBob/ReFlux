package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// deploy/reflux/install.sh against a fake release.
func TestInstallScript(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		t.Skip("the installer runs on Linux as a user")
	}
	for _, tool := range []string{"sh", "curl", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	binary := []byte("#!/bin/sh\necho 'reflux v9.9.9 (commit test)'\n")
	sum := fmt.Sprintf("%x", sha256.Sum256(binary))
	run := func(t *testing.T, sums string) (string, string, error) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/SHA256SUMS":
				io.WriteString(w, sums)
			case strings.HasPrefix(r.URL.Path, "/reflux-linux-"):
				w.Write(binary)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()
		home := t.TempDir()
		cmd := exec.Command("sh", "../../deploy/reflux/install.sh")
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/local/bin:/usr/bin:/bin", "REFLUX_BASE=" + srv.URL, "REFLUX_NO_SETUP=1"}
		out, err := cmd.CombinedOutput()
		return home, string(out), err
	}
	good := sum + "  reflux-linux-amd64\n" + sum + "  reflux-linux-arm64\n"

	home, out, err := run(t, good)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	bin := filepath.Join(home, ".local", "bin", "reflux")
	if st, err := os.Stat(bin); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("reflux not installed: %v", err)
	}
	if !strings.Contains(out, "Installed reflux v9.9.9 (commit test) to "+bin) {
		t.Errorf("output:\n%s", out)
	}
	if profile, _ := os.ReadFile(filepath.Join(home, ".profile")); !strings.Contains(string(profile), `export PATH="$HOME/.local/bin:$PATH"`) {
		t.Errorf(".profile: %q", profile)
	}

	home, out, err = run(t, strings.ReplaceAll(good, sum, strings.Repeat("0", 64)))
	if err == nil || !strings.Contains(out, "does not match SHA256SUMS") {
		t.Errorf("a wrong checksum: %v\n%s", err, out)
	}
	if fileExists(filepath.Join(home, ".local", "bin", "reflux")) {
		t.Error("installed a binary that does not match")
	}
}
