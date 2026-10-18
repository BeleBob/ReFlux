package routing

import (
	"net/netip"
	"os"
	"runtime"
	"testing"
	"time"
)

// TestRealData checks a built data file (ROUTING_DATA=<path>): it loads
// on a phone's budget and sorts well-known sites as expected.
func TestRealData(t *testing.T) {
	path := os.Getenv("ROUTING_DATA")
	if path == "" {
		t.Skip("ROUTING_DATA not set")
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	d, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	runtime.GC()
	runtime.ReadMemStats(&after)
	names, ranges := d.Counts()
	t.Logf("%d names, %d ranges, loaded in %v, heap +%d KB", names, ranges, took, (after.HeapAlloc-before.HeapAlloc)/1024)

	ru := NewRouter(Config{Mode: ModeRussia, Ads: true}, d)
	wl := NewRouter(Config{Mode: ModeWhitelist}, d)
	none := netip.Addr{}
	for _, c := range []struct {
		r    *Router
		host string
		addr string
		want Action
	}{
		{ru, "ya.ru", "", Direct},
		{ru, "gosuslugi.ru", "", Direct},
		{ru, "www.instagram.com", "", Proxy},
		{ru, "youtube.com", "", Proxy},
		{ru, "doubleclick.net", "", Block},
		{ru, "", "5.255.255.242", Direct},
		{ru, "", "1.1.1.1", Proxy},
		{wl, "online.sberbank.ru", "84.252.149.51", DirectChecked},
		{wl, "vk.com", "87.240.129.133", DirectChecked},
		{wl, "youtube.com", "142.250.74.14", Proxy},
	} {
		a := none
		if c.addr != "" {
			a = netip.MustParseAddr(c.addr)
		}
		if got := c.r.Decide(c.host, a); got != c.want {
			t.Errorf("%s: Decide(%q, %s) = %v, want %v", c.r.cfg.Mode, c.host, c.addr, got, c.want)
		}
	}
	start = time.Now()
	for i := 0; i < 100000; i++ {
		ru.Decide("static.cdn.example-something.com", netip.MustParseAddr("93.184.216.34"))
	}
	t.Logf("Decide: %v each", time.Since(start)/100000)
}
