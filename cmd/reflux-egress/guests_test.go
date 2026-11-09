package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadGuests(t *testing.T) {
	dir := t.TempDir()
	if g, err := loadGuests(dir); err != nil || g != nil {
		t.Fatalf("no file: %v, %v", g, err)
	}
	f := filepath.Join(dir, guestsFile)
	os.WriteFile(f, []byte("# test machine\n172.31.250.50\n\n 172.31.250.51 russia # second\n172.31.250.50 russia\n172.31.250.52 world\n"), 0o600)
	g, err := loadGuests(dir)
	want := []guest{{netip.MustParseAddr("172.31.250.50"), 0}, {netip.MustParseAddr("172.31.250.51"), 1}, {netip.MustParseAddr("172.31.250.52"), 0}}
	if err != nil || !slices.Equal(g, want) {
		t.Fatalf("got %v, %v", g, err)
	}
	for _, bad := range []string{"172.31.250.0/24\n", "fd00::1\n", "test\n", "172.31.250.50 uplink\n", "172.31.250.50 world x\n"} {
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
	r := guestRules([]guest{{netip.MustParseAddr("172.31.250.50"), 0}, {netip.MustParseAddr("172.31.250.51"), 1}})
	for _, want := range []string{
		"set guests {\n\t\ttype ipv4_addr\n\t\telements = { 172.31.250.50 }",
		"set guests_ru {\n\t\ttype ipv4_addr\n\t\telements = { 172.31.250.51 }",
		"type filter hook forward priority 0; policy drop;",
		`iifname "eth0" oifname "awg-world" ip saddr @guests accept`,
		`iifname "eth0" oifname "awg-ru" ip saddr @guests_ru accept`,
		`oifname "awg-world" ip saddr @guests masquerade`,
		`oifname "awg-ru" ip saddr @guests_ru masquerade`,
	} {
		if !strings.Contains(r, want) {
			t.Errorf("guest rules lack %q:\n%s", want, r)
		}
	}
	// Each set only into its own tunnel, and never out of the uplink.
	if strings.Contains(r, `oifname "eth0"`) || strings.Contains(r, `"awg-ru" ip saddr @guests `) || strings.Contains(r, `"awg-world" ip saddr @guests_ru`) {
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
		"ip route replace default dev awg-ru table 201",
		"ip route replace blackhole default metric 4000 table 201",
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

// A guest moved to the Russian tunnel leaves the world table; with Russia
// out of the uplink (ru-direct) there is no awg-ru, so its guests get only
// the blackhole, never the uplink.
func TestRussianGuests(t *testing.T) {
	f := &fakeNet{routes: "default via 172.31.250.1 dev eth0\n"}
	c := testController(t, f)
	c.confDir = t.TempDir()
	gf := filepath.Join(c.confDir, guestsFile)
	os.WriteFile(gf, []byte("172.31.250.50\n"), 0o600)
	if err := c.setup(); err != nil {
		t.Fatal(err)
	}
	n := len(f.calls)
	os.WriteFile(gf, []byte("172.31.250.50 russia\n"), 0o600)
	if err := c.setGuests(); err != nil {
		t.Fatal(err)
	}
	moved := f.calls[n:]
	del := indexOf(moved, "ip rule del from 172.31.250.50 lookup 200 priority 100")
	add := indexOf(moved, "ip rule add from 172.31.250.50 lookup 201 priority 100")
	if del < 0 || add < 0 || indexOf(moved, "ip rule add from 172.31.250.50 lookup 200") >= 0 {
		t.Errorf("guest not moved to the Russian table:\n%s", strings.Join(moved, "\n"))
	}

	f = &fakeNet{routes: "default via 172.31.250.1 dev eth0\n"}
	c = testController(t, f)
	c.confDir, c.ruDirect = filepath.Dir(gf), true
	if err := c.setup(); err != nil {
		t.Fatal(err)
	}
	if indexOf(f.calls, "ip route replace default dev awg-ru table 201") >= 0 {
		t.Error("ru-direct: a route into the missing Russian tunnel")
	}
	for _, want := range []string{"ip route replace blackhole default metric 4000 table 201", "ip rule add from 172.31.250.50 lookup 201 priority 100"} {
		if indexOf(f.calls, want) < 0 {
			t.Errorf("ru-direct: never ran %q", want)
		}
	}
	for _, call := range f.calls {
		if strings.Contains(call, "table 201") && strings.Contains(call, "eth0") {
			t.Errorf("ru-direct: guest table routed out of the uplink: %s", call)
		}
	}
}
