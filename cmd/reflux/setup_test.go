package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeSetupHost stands in for the host: commands, docker, the routing
// rules, the module, linger and the account. Commands answer from out
// (by the longest matching prefix of the command line) or fail when a
// prefix in fail matches; everything else succeeds with no output.
type fakeSetupHost struct {
	calls  []string
	out    map[string]string
	fail   []string
	docker map[string]string // docker command line → output; others fail
	onCmd  func(call string)
}

func (h *fakeSetupHost) match(m map[string]string, call string) (string, bool) {
	best, found := "", false
	var out string
	for k, v := range m {
		if strings.HasPrefix(call, k) && len(k) >= len(best) {
			best, out, found = k, v, true
		}
	}
	return out, found
}

func newFakeSetupHost(t *testing.T) *fakeSetupHost {
	h := &fakeSetupHost{out: map[string]string{}, docker: map[string]string{}}
	oldCmd, oldDocker, oldRules := runCmd, runDocker, readRules
	oldLook, oldUser, oldModule, oldLinger := lookPath, currentUser, amneziawgModule, lingerDir
	oldWait, oldLAN := egressWait, lanAddress
	runCmd = func(stdout io.Writer, name string, args ...string) error {
		call := strings.Join(append([]string{name}, args...), " ")
		h.calls = append(h.calls, call)
		if h.onCmd != nil {
			h.onCmd(call)
		}
		for _, f := range h.fail {
			if strings.HasPrefix(call, f) {
				return errors.New("failed")
			}
		}
		out, _ := h.match(h.out, call)
		io.WriteString(stdout, out)
		return nil
	}
	runDocker = func(stdout io.Writer, args ...string) error {
		call := strings.Join(args, " ")
		h.calls = append(h.calls, "docker "+call)
		out, ok := h.match(h.docker, call)
		if !ok {
			return errors.New("docker failed")
		}
		io.WriteString(stdout, out)
		return nil
	}
	readRules = func() (string, error) { return plainHostRules, nil }
	lookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	currentUser = func() string { return "owner" }
	amneziawgModule = filepath.Join(t.TempDir(), "missing")
	lingerDir = t.TempDir()
	egressWait = 0
	lanAddress = func() string { return "" }
	t.Cleanup(func() {
		runCmd, runDocker, readRules = oldCmd, oldDocker, oldRules
		lookPath, currentUser, amneziawgModule, lingerDir = oldLook, oldUser, oldModule, oldLinger
		egressWait, lanAddress = oldWait, oldLAN
	})
	return h
}

// ready makes the host a finished install.
func (h *fakeSetupHost) ready(t *testing.T, s Store) {
	h.docker["version --format"] = "27.3.1\n"
	h.docker["compose version --short"] = "2.29.7\n"
	h.docker["exec reflux-egress cat /run/reflux-egress/status.json"] = `{"world":"world-1","world_ok":true,"ru_ok":true,"ru_mode":"tunnel"}`
	h.out["crontab -l"] = "* * * * * /home/owner/.local/bin/reflux heal >> /home/owner/reflux/heal.log 2>&1\n"
	amneziawgModule = t.TempDir()
	os.WriteFile(filepath.Join(lingerDir, "owner"), nil, 0o644)
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"world-1.conf", "ru-1.conf"} {
		os.WriteFile(filepath.Join(s.Root, "egress", f), []byte(testAWGConf), 0o600)
	}
}

func (h *fakeSetupHost) ran(prefix string) bool {
	return slices.ContainsFunc(h.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

const testAWGConf = `[Interface]
PrivateKey = x
Address = 10.0.0.2/32
Jc = 4

[Peer]
PublicKey = y
Endpoint = 203.0.113.1:51820
AllowedIPs = 0.0.0.0/0
`

func newLineReader(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

func runSetup(t *testing.T, s Store, input string, args ...string) string {
	t.Helper()
	var out strings.Builder
	if err := cmdSetup(s, args, strings.NewReader(input), &out); err != nil {
		t.Fatalf("setup: %v\n%s", err, out.String())
	}
	return out.String()
}

func TestSetupCheckOnABareHostChangesNothing(t *testing.T) {
	h := newFakeSetupHost(t)
	readRules = func() (string, error) { return awgHostRules, nil }
	h.fail = []string{"id -nG", "/usr/bin/modinfo"}
	s := Store{Root: filepath.Join(t.TempDir(), "reflux")}

	out := runSetup(t, s, "", "--check")
	for _, want := range []string{
		"✗ Docker is installed, but owner may not use it",
		"✗ The amneziawg kernel module is not installed",
		"✗ owner's services stop when they log out",
		"✗ This host's own traffic goes through a VPN",
		"✗ No world tunnel configs",
		"✗ No Russian tunnel config",
		"✗ cron does not run reflux heal",
		"✗ The egress is not running yet",
		"8 step(s) to do",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
	for _, c := range h.calls {
		if strings.HasPrefix(c, "sudo") || strings.HasPrefix(c, "sh -c") || strings.HasPrefix(c, "docker compose --file") {
			t.Errorf("--check ran %q", c)
		}
	}
	if _, err := os.Stat(s.Root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--check created the data directory: %v", err)
	}
	if strings.Contains(out, "Telegram") {
		t.Errorf("--check asked about extras:\n%s", out)
	}
}

func TestSetupOnAFinishedServerOnlyReports(t *testing.T) {
	h := newFakeSetupHost(t)
	s := Store{Root: filepath.Join(t.TempDir(), "reflux")}
	h.ready(t, s)

	out := runSetup(t, s, "", "--check")
	if strings.Contains(out, "✗") {
		t.Errorf("a finished server has something missing:\n%s", out)
	}
	for _, want := range []string{"✓ Docker 27.3.1, compose 2.29.7", "✓ The amneziawg kernel module is loaded",
		"✓ owner's services run without a login", "✓ Tunnel configs: world-1.conf; Russia: ru-1.conf",
		"✓ cron runs reflux heal", "✓ The egress runs: world via world-1", "Nothing is missing."} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}

	// The full run asks only about the extras; "no" to each.
	h.calls = nil
	out = runSetup(t, s, strings.Repeat("n\n", 10))
	if h.ran("sudo") || h.ran("docker compose --file") {
		t.Errorf("a finished server was changed: %q", h.calls)
	}
	for _, want := range []string{"Set up the Telegram bot", "Back the data directory up", "Add the first client", "Setup is done"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in\n%s", want, out)
		}
	}
}

func TestSetupDockerAccess(t *testing.T) {
	for _, tc := range []struct {
		name, groups, accountGroups string
		input, want, ran            string
	}{
		{"not in the group", "owner", "owner", "y\n", "Log out and in again", "sudo usermod -aG docker owner"},
		{"added after this login", "owner", "owner docker", "", "added to the docker group after this login", ""},
		{"daemon stopped", "owner docker", "owner docker", "n\n", "its daemon does not answer", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeSetupHost(t)
			h.out["id -nG"] = tc.groups
			h.out["id -nG owner"] = tc.accountGroups
			s := Store{Root: filepath.Join(t.TempDir(), "reflux")}
			out := runSetup(t, s, tc.input)
			if !strings.Contains(out, tc.want) || !strings.Contains(out, "Run  reflux setup  again") {
				t.Errorf("output:\n%s", out)
			}
			if tc.ran != "" && !h.ran(tc.ran) {
				t.Errorf("did not run %q: %q", tc.ran, h.calls)
			}
			if h.ran("/usr/bin/modinfo") {
				t.Error("went past a Docker it cannot use")
			}
		})
	}
}

func TestSetupInstallsDockerWhenAsked(t *testing.T) {
	h := newFakeSetupHost(t)
	lookPath = func(name string) (string, error) { return "", exec.ErrNotFound }
	s := Store{Root: filepath.Join(t.TempDir(), "reflux")}
	out := runSetup(t, s, "y\n")
	if !h.ran("sh -c curl -fsSL https://get.docker.com | sudo sh") || !h.ran("sudo usermod -aG docker owner") {
		t.Errorf("calls: %q", h.calls)
	}
	if !strings.Contains(out, "log out and in again") {
		t.Errorf("output:\n%s", out)
	}
}

func TestSetupLoadsTheModule(t *testing.T) {
	h := newFakeSetupHost(t)
	err := setupModule(&setupUI{in: newLineReader("\n"), out: io.Discard}, Store{})
	if err != nil {
		t.Fatal(err)
	}
	if !h.ran("sudo modprobe amneziawg") || !h.ran("sh -c echo amneziawg | sudo tee /etc/modules-load.d/amneziawg.conf") {
		t.Errorf("calls: %q", h.calls)
	}
}

func TestSetupRouteInstallsTheUnitForTheHostVPN(t *testing.T) {
	h := newFakeSetupHost(t)
	installed := false
	readRules = func() (string, error) {
		if installed {
			return "100:\tfrom " + EgressSubnet + " lookup main\n" + awgHostRules, nil
		}
		return awgHostRules, nil
	}
	h.out["ip -o link show"] = "1: lo: <LOOPBACK,UP> mtu 65536\n5: awg0: <POINTOPOINT,NOARP,UP,LOWER_UP> mtu 1420\n"
	h.fail = []string{"systemctl is-active --quiet awg0.service"}
	var unit string
	h.onCmd = func(call string) {
		if f, ok := strings.CutPrefix(call, "sudo install -m 0644 "); ok {
			b, _ := os.ReadFile(strings.Fields(f)[0])
			unit = string(b)
		}
		if strings.HasPrefix(call, "sudo systemctl enable --now reflux-egress-route") {
			installed = true
		}
	}
	h.out["systemctl is-active --quiet awg-quick@awg0.service"] = ""
	var out strings.Builder
	u := &setupUI{in: newLineReader("\ny\n"), out: &out}
	if err := setupRoute(u, Store{}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "[awg-quick@awg0.service]") {
		t.Errorf("did not offer the VPN unit it found:\n%s", out.String())
	}
	if !strings.Contains(unit, "After=awg-quick@awg0.service\nPartOf=awg-quick@awg0.service") ||
		!strings.Contains(unit, "WantedBy=multi-user.target awg-quick@awg0.service") {
		t.Errorf("unit:\n%s", unit)
	}
	if !strings.Contains(out.String(), "✓ The egress tunnels go around the host VPN") {
		t.Errorf("output:\n%s", out.String())
	}
}

// The unit setup writes is the one in deploy/ for another VPN unit.
func TestRouteUnitMatchesTheDeployFile(t *testing.T) {
	b, err := os.ReadFile("../../deploy/reflux/host/reflux-egress-route.service")
	if err != nil {
		t.Fatal(err)
	}
	strip := func(s string) string {
		var keep []string
		for _, l := range strings.Split(s, "\n") {
			if l != "" && !strings.HasPrefix(l, "#") {
				keep = append(keep, l)
			}
		}
		return strings.Join(keep, "\n")
	}
	want := strip(strings.ReplaceAll(string(b), "awg0.service", "vpn.service"))
	if got := strip(fmt.Sprintf(routeUnit, "vpn.service")); got != want {
		t.Errorf("routeUnit differs from the deploy file:\n%s\n---\n%s", got, want)
	}
	if !strings.Contains(routeUnit, EgressSubnet) {
		t.Errorf("routeUnit is not for %s", EgressSubnet)
	}
}

func TestSetupConfigs(t *testing.T) {
	newFakeSetupHost(t)
	src := t.TempDir()
	good := filepath.Join(src, "de.conf")
	os.WriteFile(good, []byte(testAWGConf), 0o644)
	bad := filepath.Join(src, "bad.conf")
	os.WriteFile(bad, []byte("[Interface]\nPrivateKey = x\n"), 0o644)
	ru := filepath.Join(src, "msk.conf")
	os.WriteFile(ru, []byte(testAWGConf), 0o644)

	s := Store{Root: filepath.Join(t.TempDir(), "reflux")}
	var out strings.Builder
	// A bad file is refused and asked again; then two world configs, the
	// end, the Russian one, and no direct fallback (the default).
	in := strings.Join([]string{bad, good, good, "", ru, ""}, "\n") + "\n"
	if err := setupConfigs(&setupUI{in: newLineReader(in), out: &out}, s); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "is not an AmneziaWG config: it has no [Peer]") {
		t.Errorf("bad config accepted:\n%s", out.String())
	}
	dir := filepath.Join(s.Root, "egress")
	for _, f := range []string{"world-1.conf", "world-2.conf", "ru-1.conf"} {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v", f, st.Mode().Perm())
		}
	}
	if fileExists(filepath.Join(dir, "world-3.conf")) || fileExists(filepath.Join(dir, ruFallbackFile)) || fileExists(filepath.Join(dir, ruDirectFile)) {
		t.Error("wrote more than asked")
	}

	// Without a Russian config, Russia goes directly only on a yes.
	s2 := Store{Root: filepath.Join(t.TempDir(), "reflux")}
	err := setupConfigs(&setupUI{in: newLineReader(good + "\n\n\n\n"), out: io.Discard}, s2)
	if !errors.Is(err, errStop) || fileExists(filepath.Join(s2.Root, "egress", ruDirectFile)) {
		t.Errorf("direct Russia without a yes: %v", err)
	}
	if err := setupConfigs(&setupUI{in: newLineReader("\ny\n"), out: io.Discard}, s2); err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(s2.Root, "egress", ruDirectFile)) {
		t.Error("no ru-direct after a yes")
	}
}

func TestSetupCronAddsHeal(t *testing.T) {
	h := newFakeSetupHost(t)
	s := Store{Root: "/home/owner/reflux"}
	if err := setupCron(&setupUI{in: newLineReader("\n"), out: io.Discard}, s); err != nil {
		t.Fatal(err)
	}
	last := h.calls[len(h.calls)-1]
	if !strings.HasPrefix(last, "sh -c (crontab -l 2>/dev/null; echo '* * * * * ") ||
		!strings.Contains(last, " heal >> /home/owner/reflux/heal.log 2>&1') | crontab -") {
		t.Errorf("cron call: %q", last)
	}
}

func TestSetupStartsTheEgress(t *testing.T) {
	h := newFakeSetupHost(t)
	s := Store{Root: filepath.Join(t.TempDir(), "reflux")}
	h.docker["compose"] = ""
	h.docker["inspect"] = ""
	egressWait = time.Second
	started := false
	old := runDocker
	runDocker = func(stdout io.Writer, args ...string) error {
		if args[0] == "compose" {
			started = true
		}
		if started && args[0] == "exec" {
			io.WriteString(stdout, `{"world":"world-2","world_ok":true,"ru_ok":true,"ru_fallback":true}`)
			return nil
		}
		return old(stdout, args...)
	}
	var out strings.Builder
	if err := setupStart(&setupUI{in: newLineReader("\n"), out: &out}, s); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "✓ The egress runs: world via world-2, Russia "+tr(langEN, "ru.fallback")) {
		t.Errorf("output:\n%s", out.String())
	}
	if !fileExists(filepath.Join(s.Root, "compose.yml")) {
		t.Error("apply did not run")
	}
}

func TestSetupAsksAndAnswers(t *testing.T) {
	u := &setupUI{in: newLineReader("\nn\nyes\nда\nwhat\n"), out: io.Discard}
	for i, want := range []bool{true, false, true, true, true} {
		if got := u.ask("?", true); got != want {
			t.Errorf("answer %d: %v", i, got)
		}
	}
	if u.ask("?", false) {
		t.Error("end of input is not the default")
	}
}
