package main

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Guests are containers on the egress network, outside its namespace (a
// test machine, say), that the owner lets out through one of the tunnels:
// they set the egress as their gateway. The config file guests lists one
// IPv4 address per line, optionally followed by the tunnel: world (the
// default) or russia; # starts a comment. A guest goes only into its
// tunnel — never the other one, the uplink or the carriers' direct way —
// and has no way out while that tunnel is down (or, for russia, while
// Russia leaves directly: ru-direct). Nothing else is forwarded, with or
// without guests.
const (
	guestsFile    = "guests"
	guestPriority = "100"
)

// guestTunnels are the tunnels a guest may use: its keyword, interface,
// route table and nft set.
var guestTunnels = []struct{ name, iface, table, set string }{
	{"world", worldIface, "200", "guests"},
	{"russia", ruIface, "201", "guests_ru"},
}

type guest struct {
	addr   netip.Addr
	tunnel int // index in guestTunnels
}

// loadGuests reads the guests file; no file means no guests.
func loadGuests(dir string) ([]guest, error) {
	b, err := os.ReadFile(filepath.Join(dir, guestsFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []guest
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		a, err := netip.ParseAddr(f[0])
		if err != nil || !a.Is4() || len(f) > 2 {
			return nil, fmt.Errorf("%s line %d: want an IPv4 address and an optional tunnel, got %q", guestsFile, n, strings.TrimSpace(line))
		}
		t := 0
		if len(f) == 2 {
			t = slices.IndexFunc(guestTunnels, func(gt struct{ name, iface, table, set string }) bool { return gt.name == f[1] })
			if t < 0 {
				return nil, fmt.Errorf("%s line %d: tunnel %q is not world or russia", guestsFile, n, f[1])
			}
		}
		if !slices.ContainsFunc(out, func(g guest) bool { return g.addr == a }) {
			out = append(out, guest{a, t})
		}
	}
	return out, sc.Err()
}

// guestRules is the nftables table for the guests: forwarding is closed
// except from a guest into its tunnel (and the answers back), and what a
// guest sends there leaves under the tunnel's address.
func guestRules(guests []guest) string {
	var sets, fwd, nat strings.Builder
	for i, gt := range guestTunnels {
		var addrs []string
		for _, g := range guests {
			if g.tunnel == i {
				addrs = append(addrs, g.addr.String())
			}
		}
		elems := ""
		if len(addrs) > 0 {
			elems = "\n\t\telements = { " + strings.Join(addrs, ", ") + " }"
		}
		fmt.Fprintf(&sets, "\tset %s {\n\t\ttype ipv4_addr%s\n\t}\n", gt.set, elems)
		fmt.Fprintf(&fwd, "\t\tiifname %q oifname %q ip saddr @%s accept\n", uplink, gt.iface, gt.set)
		fmt.Fprintf(&nat, "\t\toifname %q ip saddr @%s masquerade\n", gt.iface, gt.set)
	}
	return `table inet reflux_guests
delete table inet reflux_guests
table inet reflux_guests {
` + sets.String() + `	chain forward {
		type filter hook forward priority 0; policy drop;
		ct state established,related accept
` + fwd.String() + `	}
	chain postrouting {
		type nat hook postrouting priority 100; policy accept;
` + nat.String() + `	}
}
`
}

// setGuests applies the guests file: the forward rules, the guests' route
// tables and one rule per guest. It is cheap to call each round: nothing
// runs unless the list (or whether Russia has a tunnel) changed.
func (c *controller) setGuests() error {
	guests, err := loadGuests(c.confDir)
	if err != nil {
		return err
	}
	if c.guestsSet && slices.Equal(guests, c.guests) && c.guestsDirect == c.ruDirect {
		return nil
	}
	if _, err := c.run(guestRules(guests), "nft", "-f", "-"); err != nil {
		return err
	}
	for _, gt := range guestTunnels {
		// No awg-ru while Russia leaves directly: its guests get only the
		// blackhole, never the uplink.
		if gt.iface == ruIface && c.ruDirect {
			c.ip("route", "del", "default", "dev", gt.iface, "table", gt.table)
		} else if err := c.ip("route", "replace", "default", "dev", gt.iface, "table", gt.table); err != nil {
			return err
		}
		if err := c.ip("route", "replace", "blackhole", "default", "metric", "4000", "table", gt.table); err != nil {
			return err
		}
	}
	for _, g := range c.guests {
		c.ip("rule", "del", "from", g.addr.String(), "lookup", guestTunnels[g.tunnel].table, "priority", guestPriority)
	}
	for _, g := range guests {
		table := guestTunnels[g.tunnel].table
		// del first: a restarted egress may find the rule already there.
		c.ip("rule", "del", "from", g.addr.String(), "lookup", table, "priority", guestPriority)
		if err := c.ip("rule", "add", "from", g.addr.String(), "lookup", table, "priority", guestPriority); err != nil {
			return err
		}
	}
	if len(guests) > 0 || c.guestsSet {
		n := make([]int, len(guestTunnels))
		for _, g := range guests {
			n[g.tunnel]++
		}
		log.Printf("guests: %d through the world tunnel, %d through the Russian one", n[0], n[1])
	}
	c.guests, c.guestsSet, c.guestsDirect = guests, true, c.ruDirect
	return nil
}
