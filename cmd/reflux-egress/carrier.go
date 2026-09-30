package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// carrierFile routes the carriers' servers, the hosts the nodes reach
// their documents at, out of the uplink: the channels' own traffic then
// skips the Russian tunnel, which carried it at 1.5 times the clients'
// downloads, while other Russian addresses still go through the tunnel.
// Its lines are the host names; an empty file means defaultCarrierHosts.
// The egress reads it when it starts.
const carrierFile = "carrier-direct"

// defaultCarrierHosts are the hosts of the Mail.ru carrier: the editor
// API and the co-authoring WebSocket.
var defaultCarrierHosts = []string{"cloud.mail.ru", "docs.datacloudmail.ru"}

const (
	// carrierEvery is how often the carrier hosts are resolved again.
	carrierEvery = time.Minute
	// carrierKeep is how long an address stays routed out after its host
	// stopped resolving to it.
	carrierKeep = 24 * time.Hour
)

// loadCarrierHosts reads carrierFile from dir: nil when it is not there.
func loadCarrierHosts(dir string) []string {
	b, err := os.ReadFile(filepath.Join(dir, carrierFile))
	if err != nil {
		return nil
	}
	var hosts []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			hosts = append(hosts, line)
		}
	}
	if len(hosts) == 0 {
		hosts = defaultCarrierHosts
	}
	return hosts
}

// resolveLocal asks the local resolver (unbound) for a host's IPv4
// addresses: the same answer the nodes get.
func resolveLocal(host string) ([]netip.Addr, error) {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, "udp", "127.0.0.1:53")
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.LookupNetIP(ctx, "ip4", host)
}

// refreshCarriers resolves the carrier hosts and routes their addresses
// out of the uplink: the kill switch lets a new address out before it is
// routed there, and an address not seen for carrierKeep goes back into the
// tunnel. A failed lookup keeps what there is: an address not routed out
// yet still reaches the carrier, through the Russian tunnel.
func (c *controller) refreshCarriers(now time.Time) error {
	if c.carriers == nil {
		c.carriers = map[netip.Addr]time.Time{}
	}
	var lookupErr error
	for _, h := range c.carrierHosts {
		addrs, err := c.lookup(h)
		if err != nil {
			lookupErr = fmt.Errorf("carrier %s: %w", h, err)
			continue
		}
		for _, a := range addrs {
			a = a.Unmap()
			if !a.Is4() {
				continue
			}
			if _, known := c.carriers[a]; !known {
				if _, err := c.run("", "nft", "add", "element", "inet", "reflux", "carrier4", "{", a.String(), "}"); err != nil {
					return err
				}
				if err := c.ip("route", "replace", a.String()+"/32", "via", c.gw.String(), "dev", uplink); err != nil {
					return err
				}
				log.Printf("carrier: %s (%s) goes out of the uplink", a, h)
			}
			c.carriers[a] = now
		}
	}
	for a, seen := range c.carriers {
		if now.Sub(seen) > carrierKeep {
			c.ip("route", "del", a.String()+"/32", "via", c.gw.String(), "dev", uplink)
			c.run("", "nft", "delete", "element", "inet", "reflux", "carrier4", "{", a.String(), "}")
			delete(c.carriers, a)
			log.Printf("carrier: %s no longer resolves: back into the tunnels", a)
		}
	}
	return lookupErr
}

// carrierAddrs lists the addresses routed out of the uplink, sorted.
func (c *controller) carrierAddrs() []string {
	var out []string
	for a := range c.carriers {
		out = append(out, a.String())
	}
	sort.Strings(out)
	return out
}
