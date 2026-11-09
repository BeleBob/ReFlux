package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadGuests(t *testing.T) {
	dir := t.TempDir()
	if g, err := loadGuests(dir); err != nil || g != nil {
		t.Fatalf("no file: %v, %v", g, err)
	}
	f := filepath.Join(dir, guestsFile)
	os.WriteFile(f, []byte("# test machine\n172.31.250.50\n\n 172.31.250.51 # second\n172.31.250.50\n"), 0o600)
	g, err := loadGuests(dir)
	if err != nil || len(g) != 2 || g[0].String() != "172.31.250.50" || g[1].String() != "172.31.250.51" {
		t.Fatalf("got %v, %v", g, err)
	}
	for _, bad := range []string{"172.31.250.0/24\n", "fd00::1\n", "test\n"} {
		os.WriteFile(f, []byte(bad), 0o600)
		if _, err := loadGuests(dir); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestGuestRulesForwardOnlyIntoTheWorldTunnel(t *testing.T) {
	none := guestRules(nil)
	if strings.Contains(none, "elements") || !strings.Contains(none, "policy drop;") {
		t.Errorf("without guests forwarding must stay closed:\n%s", none)
	}
	r := guestRules([]netip.Addr{netip.MustParseAddr("172.31.250.50")})
	for _, want := range []string{
		"elements = { 172.31.250.50 }",
		"type filter hook forward priority 0; policy drop;",
		`iifname "eth0" oifname "awg-world" ip saddr @guests accept`,
		`oifname "awg-world" ip saddr @guests masquerade`,
	} {
		if !strings.Contains(r, want) {
			t.Errorf("guest rules lack %q:\n%s", want, r)
		}
	}
	// Never the Russian tunnel or the uplink.
	if strings.Contains(r, "awg-ru") || strings.Contains(r, `oifname "eth0"`) {
		t.Errorf("guests may leave elsewhere:\n%s", r)
	}
}

func TestSetupRoutesGuestsIntoTheWorldTunnel(t *testing.T) {
	f := &fakeNet{routes: "default via 172.31.250.1 dev eth0\n"}
	c := testController(t, f)
	c.confDir = t.TempDir()
	os.WriteFile(filepath.Join(c.confDir, guestsFile), []byte("172.31.250.50\n"), 0o600)
	if err := c.setup(); err != nil {
		t.Fatal(err)
	}
	world := indexOf(f.calls, "ip route replace default dev awg-world metric 0")
	rules := -1
	for i, call := range f.calls {
		if call == "nft -f -" && strings.Contains(f.stdin[i], "reflux_guests") {
			rules = i
		}
	}
	if world < 0 || rules < world {
		t.Errorf("guest rules %d before the world route %d:\n%s", rules, world, strings.Join(f.calls, "\n"))
	}
	for _, want := range []string{
		"ip route replace default dev awg-world table 200",
		"ip route replace blackhole default metric 4000 table 200",
		"ip rule add from 172.31.250.50 lookup 200 priority 100",
	} {
		if indexOf(f.calls, want) < 0 {
			t.Errorf("setup never ran %q", want)
		}
	}

	// Each round reapplies only a changed list; a removed guest loses its rule.
	n := len(f.calls)
	if err := c.setGuests(); err != nil || len(f.calls) != n {
		t.Fatalf("unchanged list reapplied: %v\n%s", err, strings.Join(f.calls[n:], "\n"))
	}
	os.WriteFile(filepath.Join(c.confDir, guestsFile), []byte("# none\n"), 0o600)
	if err := c.setGuests(); err != nil {
		t.Fatal(err)
	}
	if indexOf(f.calls[n:], "ip rule del from 172.31.250.50 lookup 200 priority 100") < 0 {
		t.Errorf("removed guest kept its rule:\n%s", strings.Join(f.calls[n:], "\n"))
	}
	if indexOf(f.calls[n:], "ip rule add") >= 0 {
		t.Error("a rule was added for no guest")
	}
}

func TestSetupWithoutGuestsStillClosesForwarding(t *testing.T) {
	f := &fakeNet{routes: "default via 172.31.250.1 dev eth0\n"}
	c := testController(t, f)
	c.confDir = t.TempDir()
	if err := c.setup(); err != nil {
		t.Fatal(err)
	}
	closed := false
	for i, call := range f.calls {
		if call == "nft -f -" && strings.Contains(f.stdin[i], "reflux_guests") && strings.Contains(f.stdin[i], "policy drop;") {
			closed = true
		}
	}
	if !closed || indexOf(f.calls, "ip rule add") >= 0 {
		t.Errorf("forwarding not closed or a guest rule added:\n%s", strings.Join(f.calls, "\n"))
	}
}
