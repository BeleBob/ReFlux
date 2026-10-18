package mobile

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/routing"
	"github.com/p1neappleXpress/OpenFlux/socks5"
)

// routerDialer is the proxy mode's routing: the SOCKS5 server asks it for
// each connection, by name when the app gave one. Direct connections are
// opened from the app, the rest through the tunnel.
type routerDialer struct {
	tunnel socks5.Dialer
	router *routing.Router
}

var errBlocked = errors.New("blocked by the routing settings")

// resolveDirect looks a whitelisted name up outside the tunnel, to check
// its address against the whitelist; tests replace it.
var resolveDirect = func(host string) (netip.Addr, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil || len(addrs) == 0 {
		return netip.Addr{}, fmt.Errorf("resolve %s: %v", host, err)
	}
	return addrs[0], nil
}

func (d *routerDialer) decide(address string) (routing.Action, string) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return routing.Proxy, ""
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return d.router.Decide("", a.Unmap()), ""
	}
	host = strings.ToLower(host)
	action := d.router.Decide(host, netip.Addr{})
	if action == routing.Proxy && d.router.Config().Mode == routing.ModeWhitelist &&
		d.router.WhitelistedName(host) && !d.router.Failed(host) {
		// Only an address can tell whether a whitelisted name goes direct.
		if a, err := resolveDirect(host); err == nil {
			action = d.router.Decide(host, a)
		}
	}
	return action, host
}

func (d *routerDialer) DialTCP(address string) (net.Conn, error) {
	action, host := d.decide(address)
	switch action {
	case routing.Block:
		return nil, errBlocked
	case routing.Direct:
		return dialDirect("tcp", address, directDialTimeout)
	case routing.DirectChecked:
		c, err := dialDirect("tcp", address, checkedDialTimeout)
		if err == nil {
			return c, nil
		}
		d.router.MarkFailed(host)
	}
	return d.tunnel.DialTCP(address)
}

func (d *routerDialer) DialUDP(address string) (net.Conn, error) {
	action, _ := d.decide(address)
	switch action {
	case routing.Block:
		return nil, errBlocked
	case routing.Direct:
		return dialDirect("udp", address, 5*time.Second)
	}
	udp, ok := d.tunnel.(socks5.UDPDialer)
	if !ok {
		return nil, fmt.Errorf("tunnel dialer does not support UDP")
	}
	return udp.DialUDP(address)
}
