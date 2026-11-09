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
// test machine, say), that the owner lets out through the world tunnel:
// they set the egress as their gateway. The config file guests lists one
// IPv4 address per line (# starts a comment). A guest goes only into the
// world tunnel — never the Russian one, the uplink or the carriers' direct
// way — and has no way out while the world tunnel is down. Nothing else is
// forwarded, with or without guests.
const (
	guestsFile = "guests"
	// guestTable routes the guests: the world tunnel, else nothing.
	guestTable    = "200"
	guestPriority = "100"
)

// loadGuests reads the guests file; no file means no guests.
func loadGuests(dir string) ([]netip.Addr, error) {
	b, err := os.ReadFile(filepath.Join(dir, guestsFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		a, err := netip.ParseAddr(line)
		if err != nil || !a.Is4() {
			return nil, fmt.Errorf("%s line %d: %q is not an IPv4 address", guestsFile, n, line)
		}
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out, sc.Err()
}

// guestRules is the nftables table for the guests: forwarding is closed
// except from a guest into the world tunnel (and the answers back), and
// what a guest sends there leaves under the tunnel's address.
func guestRules(guests []netip.Addr) string {
	elems := ""
	if len(guests) > 0 {
		s := make([]string, len(guests))
		for i, a := range guests {
			s[i] = a.String()
		}
		elems = "\n\t\telements = { " + strings.Join(s, ", ") + " }"
	}
	return `table inet reflux_guests
delete table inet reflux_guests
table inet reflux_guests {
	set guests {
		type ipv4_addr` + elems + `
	}
	chain forward {
		type filter hook forward priority 0; policy drop;
		ct state established,related accept
		iifname "` + uplink + `" oifname "` + worldIface + `" ip saddr @guests accept
	}
	chain postrouting {
		type nat hook postrouting priority 100; policy accept;
		oifname "` + worldIface + `" ip saddr @guests masquerade
	}
}
`
}

// setGuests applies the guests file: the forward rules, the guests' route
// table and one rule per guest. It is cheap to call each round: nothing
// runs unless the list changed.
func (c *controller) setGuests() error {
	guests, err := loadGuests(c.confDir)
	if err != nil {
		return err
	}
	if c.guestsSet && slices.Equal(guests, c.guests) {
		return nil
	}
	if _, err := c.run(guestRules(guests), "nft", "-f", "-"); err != nil {
		return err
	}
	if err := c.ip("route", "replace", "default", "dev", worldIface, "table", guestTable); err != nil {
		return err
	}
	if err := c.ip("route", "replace", "blackhole", "default", "metric", "4000", "table", guestTable); err != nil {
		return err
	}
	for _, a := range c.guests {
		if !slices.Contains(guests, a) {
			c.ip("rule", "del", "from", a.String(), "lookup", guestTable, "priority", guestPriority)
		}
	}
	for _, a := range guests {
		// del first: a restarted egress may find the rule already there.
		c.ip("rule", "del", "from", a.String(), "lookup", guestTable, "priority", guestPriority)
		if err := c.ip("rule", "add", "from", a.String(), "lookup", guestTable, "priority", guestPriority); err != nil {
			return err
		}
	}
	if len(guests) > 0 || c.guestsSet {
		log.Printf("guests: %d out through the world tunnel", len(guests))
	}
	c.guests, c.guestsSet = guests, true
	return nil
}
