package mobile

import (
	"fmt"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/p1neappleXpress/OpenFlux/routing"
)

// Routing: the app's "Маршрутизация". In the tunnel mode (Start /
// StartSession) every packet the device sends comes through Send; with
// routing on, the packets of connections that go direct or nowhere are
// taken aside into a small TCP/IP stack of the phone's own (routing_stack.go),
// which opens them from the app itself: the app's sockets stay outside the
// VPN. Everything else goes into the tunnel untouched, as without routing.
//
// The router needs the site behind each address. It learns it from the DNS
// answers coming back through the tunnel; so that apps ask the device's
// resolver, not their own encrypted DNS, the VPN's DNS server is RoutingDNS
// (an address only this package answers): its questions go to the chosen
// upstream through the tunnel, and ad names get a blocked answer.

// RoutingDNSAddress is the VPN's DNS server while routing is on: inside the
// app's TUN subnet (10.10.10.0/24), unknown to the browsers' encrypted DNS
// lists, so Chrome does not swap it for DNS over HTTPS.
const RoutingDNSAddress = "10.10.10.53"

var routingDNS = netip.MustParseAddr(RoutingDNSAddress)

var routingSetup struct {
	mu       sync.Mutex
	cfg      routing.Config
	dataPath string
	upstream netip.Addr

	// The loaded data, kept while the file does not change.
	data      *routing.Data
	dataStamp string
}

// active is the router of the running tunnel, nil without routing.
var active atomic.Pointer[packetRouter]

// SetRouting sets the routing for the next start: configJSON is
// {"mode": "tunnel"|"russia"|"whitelist", "ads": bool, "direct": [sites]},
// dataPath the routing data file (routing.Load), dnsServer the IPv4 DNS
// server the device's questions go to through the tunnel. It returns "" or
// what is wrong.
func SetRouting(configJSON, dataPath, dnsServer string) string {
	cfg, err := routing.ParseConfig(configJSON)
	if err != nil {
		return err.Error()
	}
	upstream := netip.MustParseAddr("1.1.1.1")
	if dnsServer != "" {
		a, err := netip.ParseAddr(dnsServer)
		if err != nil || !a.Is4() {
			return fmt.Sprintf("routing: DNS server %q is not an IPv4 address", dnsServer)
		}
		upstream = a
	}
	routingSetup.mu.Lock()
	routingSetup.cfg, routingSetup.dataPath, routingSetup.upstream = cfg, dataPath, upstream
	routingSetup.mu.Unlock()
	return ""
}

// RoutingActive reports whether the next start routes: then the VPN's DNS
// server must be RoutingDNS.
func RoutingActive() bool {
	routingSetup.mu.Lock()
	defer routingSetup.mu.Unlock()
	return routingSetup.cfg.Active()
}

// RoutingDNS is the DNS server the VPN must announce while routing is on.
func RoutingDNS() string { return RoutingDNSAddress }

// RoutingDataInfo describes a data file: "" when it does not load, else
// its build time and size, for the settings screen.
func RoutingDataInfo(path string) string {
	d, err := routing.LoadFile(path)
	if err != nil {
		return ""
	}
	names, ranges := d.Counts()
	return fmt.Sprintf(`{"generated":%q,"names":%d,"ranges":%d}`, d.Generated, names, ranges)
}

// newRouter builds the router for a start from the settings, or nil when
// routing is off. A data file that does not load leaves only the person's
// own sites and the private addresses known, and says so in the log.
func newRouter() *routing.Router {
	routingSetup.mu.Lock()
	defer routingSetup.mu.Unlock()
	cfg := routingSetup.cfg
	if !cfg.Active() {
		return nil
	}
	path := routingSetup.dataPath
	if path != "" {
		st, err := os.Stat(path)
		stamp := ""
		if err == nil {
			stamp = fmt.Sprintf("%s %d %d", path, st.Size(), st.ModTime().UnixNano())
		}
		if stamp != routingSetup.dataStamp || routingSetup.data == nil {
			start := time.Now()
			d, err := routing.LoadFile(path)
			if err != nil {
				appendLog(fmt.Sprintf("[ERROR] Маршрутизация: списки не загрузились (%v), известны только свои сайты", err))
				routingSetup.data, routingSetup.dataStamp = nil, ""
			} else {
				names, ranges := d.Counts()
				appendLog(fmt.Sprintf("[ANDROID] Маршрутизация: списки от %s, сайтов %d, диапазонов %d, загружены за %v",
					d.Generated, names, ranges, time.Since(start).Round(time.Millisecond)))
				routingSetup.data, routingSetup.dataStamp = d, stamp
			}
		}
	}
	appendLog(fmt.Sprintf("[ANDROID] Маршрутизация: режим %s, реклама %v, своих сайтов %d", cfg.Mode, cfg.Ads, len(cfg.Direct)))
	return routing.NewRouter(cfg, routingSetup.data)
}

func routingUpstream() netip.Addr {
	routingSetup.mu.Lock()
	defer routingSetup.mu.Unlock()
	if !routingSetup.upstream.IsValid() {
		return netip.MustParseAddr("1.1.1.1")
	}
	return routingSetup.upstream
}
