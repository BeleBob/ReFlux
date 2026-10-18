package routing

import (
	"bytes"
	"compress/gzip"
	"net/netip"
	"testing"
	"time"
)

const testData = `reflux-routing 1
generated 2026-10-18T08:17:03Z
@site category-ru
s yandex.ru
s sberbank.ru
f only.example.com
k sberbank
@site category-gov-ru
s gosuslugi.ru
@site ru-blocked
s blocked.ru
s linkedin.com
@site category-ads-all
s ads.example.com
r ^track[0-9]+\.example\.net$
@site win-spy
s telemetry.microsoft.com
@site whitelist
s sberbank.ru
s vk.com
@ip ru
5.255.0.0/16
77.88.0.0/18
@ip private
10.0.0.0/8
@ip whitelist
87.240.128.0/18
84.252.149.206
`

func gz(t *testing.T, s string) *bytes.Buffer {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return &b
}

func load(t *testing.T) *Data {
	t.Helper()
	d, err := Load(gz(t, testData))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestLoadAndMatch(t *testing.T) {
	d := load(t)
	if d.Generated != "2026-10-18T08:17:03Z" {
		t.Errorf("generated %q", d.Generated)
	}
	for _, c := range []struct {
		list, host string
		want       bool
	}{
		{SiteRussia, "yandex.ru", true},
		{SiteRussia, "mail.YANDEX.ru.", true},
		{SiteRussia, "notyandex.ru", false},
		{SiteRussia, "only.example.com", true},
		{SiteRussia, "sub.only.example.com", false},
		{SiteRussia, "online-sberbank-cdn.net", true}, // keyword
		{SiteAds, "x.ads.example.com", true},
		{SiteAds, "track12.example.net", true},
		{SiteAds, "trackx.example.net", false},
		{"no-such-list", "yandex.ru", false},
	} {
		if got := d.InSite(c.list, c.host); got != c.want {
			t.Errorf("InSite(%s, %s) = %v", c.list, c.host, got)
		}
	}
	for _, c := range []struct {
		list, addr string
		want       bool
	}{
		{IPRussia, "5.255.255.242", true},
		{IPRussia, "77.88.63.255", true},
		{IPRussia, "77.88.64.0", false},
		{IPWhitelist, "84.252.149.206", true},
		{IPWhitelist, "84.252.149.207", false},
		{IPWhitelist, "::ffff:87.240.129.133", true},
	} {
		if got := d.InIP(c.list, netip.MustParseAddr(c.addr)); got != c.want {
			t.Errorf("InIP(%s, %s) = %v", c.list, c.addr, got)
		}
	}
	if _, err := Load(gz(t, "something else\n")); err == nil {
		t.Error("a file without the magic line loaded")
	}
}

func TestIPRangesMerge(t *testing.T) {
	var s ipSet
	for _, p := range []string{"10.0.0.0/24", "10.0.1.0/24", "10.0.0.128/25", "255.255.255.0/24", "255.255.255.255/32", "1.1.1.1/32"} {
		s.add(netip.MustParsePrefix(p))
	}
	s.finish()
	if len(s.lo) != 3 {
		t.Errorf("ranges %d, want 3 (merged): %v %v", len(s.lo), s.lo, s.hi)
	}
	for addr, want := range map[string]bool{"10.0.1.255": true, "10.0.2.0": false, "255.255.255.255": true, "1.1.1.1": true, "1.1.1.2": false} {
		if s.contains(netip.MustParseAddr(addr)) != want {
			t.Errorf("contains %s != %v", addr, want)
		}
	}
}

func TestRussiaMode(t *testing.T) {
	r := NewRouter(Config{Mode: ModeRussia, Ads: true, Direct: []string{"warthunder.com"}}, load(t))
	none := netip.Addr{}
	for _, c := range []struct {
		host, addr string
		want       Action
	}{
		{"yandex.ru", "", Direct},
		{"gosuslugi.ru", "", Direct},
		{"blocked.ru", "5.255.1.1", Proxy},        // blocked beats a Russian address
		{"", "5.255.1.1", Direct},                 // a Russian address
		{"example.org", "8.8.8.8", Proxy},         // the rest
		{"x.ads.example.com", "8.8.8.8", Block},   // ads
		{"telemetry.microsoft.com", "", Block},    // win-spy
		{"cdn.warthunder.com", "8.8.8.8", Direct}, // own
		{"", "10.1.2.3", Direct},                  // private
		{"", "192.168.1.1", Direct},
	} {
		a := none
		if c.addr != "" {
			a = netip.MustParseAddr(c.addr)
		}
		if got := r.Decide(c.host, a); got != c.want {
			t.Errorf("Decide(%q, %s) = %v, want %v", c.host, c.addr, got, c.want)
		}
	}
	if !r.BlockName("ads.example.com") || r.BlockName("yandex.ru") {
		t.Error("BlockName")
	}
	off := NewRouter(Config{Mode: ModeRussia}, load(t))
	if off.BlockName("ads.example.com") || off.Decide("ads.example.com", none) == Block {
		t.Error("ads blocked with the switch off")
	}
}

func TestWhitelistMode(t *testing.T) {
	r := NewRouter(Config{Mode: ModeWhitelist}, load(t))
	now := time.Unix(1_800_000_000, 0)
	r.now = func() time.Time { return now }
	sber := netip.MustParseAddr("84.252.149.206")
	vk := netip.MustParseAddr("87.240.129.133")
	for _, c := range []struct {
		host string
		addr netip.Addr
		want Action
	}{
		{"sberbank.ru", sber, DirectChecked},
		{"vk.com", vk, DirectChecked},
		{"vk.com", netip.MustParseAddr("1.2.3.4"), Proxy},          // the address is not on the list
		{"yandex.ru", netip.MustParseAddr("5.255.255.242"), Proxy}, // the name is not
		{"", vk, Proxy}, // no name
	} {
		if got := r.Decide(c.host, c.addr); got != c.want {
			t.Errorf("Decide(%q, %s) = %v, want %v", c.host, c.addr, got, c.want)
		}
	}
	r.MarkFailed("sberbank.ru")
	if r.Decide("sberbank.ru", sber) != Proxy {
		t.Error("a failed site still goes direct")
	}
	now = now.Add(failedFor)
	if r.Decide("sberbank.ru", sber) != DirectChecked {
		t.Error("a failed site never goes back to direct")
	}
}

func TestTunnelModeAndConfig(t *testing.T) {
	c, err := ParseConfig("")
	if err != nil || c.Mode != ModeTunnel || c.Active() {
		t.Fatalf("empty config %+v %v", c, err)
	}
	c, err = ParseConfig(`{"mode":"tunnel","ads":true,"direct":[" Example.COM. ",""]}`)
	if err != nil || !c.Active() || len(c.Direct) != 1 || c.Direct[0] != "example.com" {
		t.Fatalf("config %+v %v", c, err)
	}
	if _, err := ParseConfig(`{"mode":"everything"}`); err == nil {
		t.Error("unknown mode accepted")
	}
	r := NewRouter(c, load(t))
	if r.Decide("yandex.ru", netip.MustParseAddr("5.255.255.242")) != Proxy {
		t.Error("tunnel mode sent a Russian site direct")
	}
	if r.Decide("www.example.com", netip.Addr{}) != Direct {
		t.Error("own site not direct")
	}
	// Without data only the own sites and private addresses are known.
	bare := NewRouter(Config{Mode: ModeRussia}, nil)
	if bare.Decide("yandex.ru", netip.MustParseAddr("5.255.255.242")) != Proxy || bare.Decide("", netip.MustParseAddr("10.0.0.1")) != Direct {
		t.Error("router without data")
	}
}
