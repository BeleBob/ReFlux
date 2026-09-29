package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// awgConf is one AmneziaWG client config as a provider exports it (the
// wg-quick format), split into what `awg setconf` takes and what the
// controller applies itself: the address, the MTU and the endpoint.
type awgConf struct {
	Name     string         // file name, for logs and status
	Address  netip.Prefix   // the tunnel's IPv4 address
	MTU      int            // 0: the controller's default
	Endpoint netip.AddrPort // the server; also the only hole in the kill switch
	SetConf  string         // the config without wg-quick's keys
}

// quickOnly are wg-quick's keys: `awg setconf` rejects them. The controller
// applies Address and MTU itself and ignores the rest: DNS is the egress
// resolver's job, and no script from a config file is ever run.
var quickOnly = map[string]bool{
	"address": true, "dns": true, "mtu": true, "table": true, "saveconfig": true,
	"preup": true, "postup": true, "predown": true, "postdown": true,
	// The controller sets the port itself: see listenPort.
	"listenport": true,
}

func parseAWG(name string, data []byte) (awgConf, error) {
	c := awgConf{Name: name}
	var out bytes.Buffer
	section := ""
	peers := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			switch section {
			case "interface":
				out.WriteString("[Interface]\n")
			case "peer":
				peers++
				out.WriteString("[Peer]\n")
			default:
				return c, fmt.Errorf("%s:%d: unknown section [%s]", name, n, section)
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return c, fmt.Errorf("%s:%d: expected key = value", name, n)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		lower := strings.ToLower(key)
		if section == "" {
			return c, fmt.Errorf("%s:%d: %s outside a section", name, n, key)
		}
		if section == "interface" && quickOnly[lower] {
			switch lower {
			case "address":
				if err := c.setAddress(value); err != nil {
					return c, fmt.Errorf("%s:%d: %w", name, n, err)
				}
			case "mtu":
				mtu, err := strconv.Atoi(value)
				if err != nil || mtu < 1280 || mtu > 1500 {
					return c, fmt.Errorf("%s:%d: bad MTU %q", name, n, value)
				}
				c.MTU = mtu
			}
			continue
		}
		if section == "peer" && lower == "endpoint" {
			ep, err := netip.ParseAddrPort(value)
			if err != nil || !ep.Addr().Is4() {
				return c, fmt.Errorf("%s:%d: Endpoint must be an IPv4 address with a port, got %q", name, n, value)
			}
			c.Endpoint = ep
		}
		fmt.Fprintf(&out, "%s = %s\n", key, value)
	}
	if err := sc.Err(); err != nil {
		return c, err
	}
	switch {
	case peers != 1:
		return c, fmt.Errorf("%s: want exactly one [Peer], found %d", name, peers)
	case !c.Address.IsValid():
		return c, fmt.Errorf("%s: no IPv4 Address", name)
	case !c.Endpoint.IsValid():
		return c, fmt.Errorf("%s: no Endpoint", name)
	}
	c.SetConf = out.String()
	return c, nil
}

// setAddress keeps the first IPv4 address: the egress is IPv4 only.
func (c *awgConf) setAddress(value string) error {
	for _, a := range strings.Split(value, ",") {
		p, err := netip.ParsePrefix(strings.TrimSpace(a))
		if err != nil {
			return fmt.Errorf("bad Address %q", a)
		}
		if p.Addr().Is4() && !c.Address.IsValid() {
			c.Address = p
		}
	}
	return nil
}

// withListenPort returns the setconf text with the interface's UDP port
// fixed, so the tunnels keep one NAT mapping across failovers and restarts
// instead of a random port each time.
func (c awgConf) withListenPort(port int) string {
	return strings.Replace(c.SetConf, "[Interface]\n", fmt.Sprintf("[Interface]\nListenPort = %d\n", port), 1)
}
