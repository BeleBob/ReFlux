package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ruIface    = "awg-ru"
	worldIface = "awg-world"

	// Fixed UDP ports of the tunnels (see awgConf.withListenPort).
	ruPort    = 51821
	worldPort = 51822

	uplink     = "eth0"
	defaultMTU = 1380

	// A world tunnel is dropped for the next one after this many probe
	// rounds in a row fail; a round is probeEvery apart.
	failLimit  = 3
	probeEvery = 10 * time.Second

	// With a direct fallback, Russia moves back into its tunnel once the
	// tunnel has answered this many rounds in a row (5 minutes): each move
	// breaks the connections to Russian addresses, the carriers' included.
	recoverRounds = 30
)

// Probe targets: answered by anycast resolvers worldwide, and outside the
// RU prefixes, so they leave through the world tunnel. ruProbes are inside
// them and leave through the Russian one. Some providers drop ICMP, so the
// world tunnel also counts as up when a TCP connection to worldTCP opens
// (port 443: port 53 would be redirected to the local resolver).
var (
	worldProbes = []string{"9.9.9.9", "1.1.1.1"}
	worldTCP    = []string{"1.1.1.1", "9.9.9.9"}
	ruProbes    = []string{"77.88.8.8"}
)

// runner runs a command with stdin and returns its combined output.
type runner func(stdin, name string, args ...string) (string, error)

func execRunner(stdin, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

type controller struct {
	run     runner
	runDir  string // setconf files and status.json
	confDir string // the configs, and the owner's choice of world server

	ru         awgConf
	ruDirect   bool // Russia leaves through the uplink, never a tunnel
	ruFallback bool // Russia leaves through the uplink while its tunnel is down
	world      []awgConf
	cur        int    // index into world of the active tunnel
	picked     string // the world server choice last acted on
	gw         netip.Addr

	// routeMu serializes the changes of the Russian routes: the prefix
	// refresh and the moves between the tunnel and the uplink.
	routeMu  sync.Mutex
	onDirect bool // the RU prefixes are routed out of the uplink now

	mu       sync.Mutex
	prefixes []netip.Prefix // RU prefixes routed to Russia
	st       status
}

// status is what `reflux-egress status` prints and the healthcheck reads.
type status struct {
	Updated    time.Time `json:"updated"`
	World      string    `json:"world"` // config file of the active world tunnel
	WorldOK    bool      `json:"world_ok"`
	WorldSince time.Time `json:"world_since"`
	Selected   string    `json:"world_selected,omitempty"` // the owner's choice, if any
	Failures   int       `json:"world_failures"`
	RUOK       bool      `json:"ru_ok"`
	RUMode     string    `json:"ru_mode"`               // "tunnel" or "direct": the way in use
	RUFallback bool      `json:"ru_fallback,omitempty"` // direct because the tunnel is down
	RUPrefixes int       `json:"ru_prefixes"`
	RUListAt   time.Time `json:"ru_list_updated"`
	Dropped    int64     `json:"killswitch_dropped"` // packets the kill switch stopped
	Error      string    `json:"error,omitempty"`
}

// directFile in the config directory sends Russian addresses out of the
// uplink (the host's own provider) instead of a Russian tunnel;
// fallbackFile keeps the tunnel and sends them out of the uplink only while
// the tunnel does not answer.
const (
	directFile   = "ru-direct"
	fallbackFile = "ru-fallback-direct"
)

// selectFile names the world server the owner chose (reflux gateway, or
// the bot). The egress starts with it and switches to it when the file
// changes; when that server fails, failover goes on in file order.
const selectFile = "world-select"

// selection is the world server the owner chose, "" for none.
func (c *controller) selection() string {
	b, err := os.ReadFile(filepath.Join(c.confDir, selectFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (c *controller) worldIndex(name string) int {
	for i, w := range c.world {
		if w.Name == name {
			return i
		}
	}
	return -1
}

// loadConfigs reads world-*.conf (in name order: the failover order) and,
// unless directFile is there, ru-*.conf (the first one is used) from dir.
func loadConfigs(dir string) (ru awgConf, direct, fallback bool, world []awgConf, err error) {
	read := func(pattern string) ([]awgConf, error) {
		files, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			return nil, err
		}
		sort.Strings(files)
		var out []awgConf
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			c, err := parseAWG(filepath.Base(f), b)
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		return out, nil
	}
	if world, err = read("world-*.conf"); err != nil {
		return awgConf{}, false, false, nil, err
	}
	if len(world) == 0 {
		return awgConf{}, false, false, nil, fmt.Errorf("%s needs at least one world-*.conf", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, directFile)); err == nil {
		return awgConf{}, true, false, world, nil
	}
	rus, err := read("ru-*.conf")
	if err != nil {
		return awgConf{}, false, false, nil, err
	}
	if len(rus) == 0 {
		return awgConf{}, false, false, nil, fmt.Errorf("%s needs a ru-*.conf, or a %s file to send Russia out directly", dir, directFile)
	}
	_, err = os.Stat(filepath.Join(dir, fallbackFile))
	return rus[0], false, err == nil, world, nil
}

// carrierDNS are the resolvers unbound asks for the carriers' domains
// (Yandex DNS, by the Russian route): the DNS redirect must let unbound's
// own queries to them through, or it would answer itself.
const carrierDNS = "77.88.8.8, 77.88.8.1"

// killSwitch is the nftables ruleset applied before any tunnel exists:
// out of the uplink only UDP to the active tunnel servers may leave, so a
// packet either goes through a tunnel or nowhere. When Russia may leave
// directly (always, or as a fallback), packets to the addresses in set ru4
// may leave too; the set is filled only while Russia is routed out of the
// uplink. Every DNS query, to any address, is answered by the local
// resolver.
func killSwitch(ruDirect bool) string {
	direct := ""
	if ruDirect {
		direct = "\n\t\toifname \"" + uplink + "\" ip daddr @ru4 accept"
	}
	return `table inet reflux
delete table inet reflux
table inet reflux {
	set endpoints {
		type ipv4_addr
	}
	set ru4 {
		type ipv4_addr
		flags interval
	}
	chain output {
		type filter hook output priority 0; policy accept;
		oifname "lo" accept
		oifname "` + uplink + `" ip daddr @endpoints meta l4proto udp accept` + direct + `
		oifname "` + uplink + `" counter drop
	}
	chain dns {
		type nat hook output priority -150; policy accept;
		ip daddr { ` + carrierDNS + ` } meta l4proto { udp, tcp } th dport 53 accept
		ip daddr != 127.0.0.1 udp dport 53 redirect to :53
		ip daddr != 127.0.0.1 tcp dport 53 redirect to :53
	}
}
`
}

// routeBatch is an `ip -batch` script routing add to target ("dev awg-ru"
// or "via <gw> dev eth0") and removing del.
func routeBatch(add, del []netip.Prefix, target string) string {
	var b strings.Builder
	for _, p := range del {
		fmt.Fprintf(&b, "route del %s %s\n", p, target)
	}
	for _, p := range add {
		fmt.Fprintf(&b, "route replace %s %s\n", p, target)
	}
	return b.String()
}

// setBatch is an `nft -f` script adding add to and removing del from a
// set, a few hundred elements a line.
func setBatch(verb, set string, ps []netip.Prefix) string {
	var b strings.Builder
	for i := 0; i < len(ps); i += 500 {
		j := min(i+500, len(ps))
		parts := make([]string, j-i)
		for k, p := range ps[i:j] {
			parts[k] = p.String()
		}
		fmt.Fprintf(&b, "%s element inet reflux %s { %s }\n", verb, set, strings.Join(parts, ", "))
	}
	return b.String()
}

// ruTarget is where Russian prefixes are routed. The caller holds routeMu.
func (c *controller) ruTarget() string {
	if c.onDirect {
		return "via " + c.gw.String() + " dev " + uplink
	}
	return "dev " + ruIface
}

func (c *controller) ip(args ...string) error {
	_, err := c.run("", "ip", args...)
	return err
}

// defaultGateway is the uplink's gateway, read before the default route
// through it is removed.
func (c *controller) defaultGateway() (netip.Addr, error) {
	out, err := c.run("", "ip", "-4", "route", "show", "default", "dev", uplink)
	if err != nil {
		return netip.Addr{}, err
	}
	f := strings.Fields(out)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "via" {
			return netip.ParseAddr(f[i+1])
		}
	}
	return netip.Addr{}, fmt.Errorf("no default gateway on %s in %q", uplink, strings.TrimSpace(out))
}

// setup brings the namespace from Docker's default (a route out of the
// uplink) to egress-only. The kill switch goes first, so nothing leaves
// in between.
func (c *controller) setup() error {
	c.onDirect = c.ruDirect
	if _, err := c.run(killSwitch(c.ruDirect || c.ruFallback), "nft", "-f", "-"); err != nil {
		return err
	}
	// A retry after a partial setup finds the route already gone: keep the
	// gateway from the first attempt.
	if !c.gw.IsValid() {
		gw, err := c.defaultGateway()
		if err != nil {
			return err
		}
		c.gw = gw
	}
	// Without a tunnel, nothing is reachable: the blackhole catches what
	// the tunnels' routes do not.
	if err := c.ip("route", "replace", "blackhole", "default", "metric", "4000"); err != nil {
		return err
	}
	c.ip("route", "del", "default", "via", c.gw.String(), "dev", uplink)
	if gw, err := c.defaultGateway(); err == nil {
		return fmt.Errorf("default route via %s on %s is still there", gw, uplink)
	}
	if !c.ruDirect {
		if err := c.up(ruIface, c.ru); err != nil {
			return err
		}
	}
	if err := c.up(worldIface, c.world[c.cur]); err != nil {
		return err
	}
	return c.ip("route", "replace", "default", "dev", worldIface, "metric", "0")
}

// up creates iface for conf, or moves it to conf when it exists.
func (c *controller) up(iface string, conf awgConf) error {
	if _, err := c.run("", "ip", "link", "show", iface); err != nil {
		if err := c.ip("link", "add", iface, "type", "amneziawg"); err != nil {
			return fmt.Errorf("%w (is the amneziawg kernel module loaded on the host?)", err)
		}
	}
	if err := c.openEndpoint(conf.Endpoint.Addr()); err != nil {
		return err
	}
	port := worldPort
	if iface == ruIface {
		port = ruPort
	}
	f := filepath.Join(c.runDir, iface+".conf")
	if err := os.WriteFile(f, []byte(conf.withListenPort(port)), 0o600); err != nil {
		return err
	}
	_, err := c.run("", "awg", "setconf", iface, f)
	os.Remove(f)
	if err != nil {
		return err
	}
	if err := c.ip("address", "flush", "dev", iface); err != nil {
		return err
	}
	if err := c.ip("address", "add", conf.Address.String(), "dev", iface); err != nil {
		return err
	}
	mtu := conf.MTU
	if mtu == 0 {
		mtu = defaultMTU
	}
	return c.ip("link", "set", iface, "mtu", fmt.Sprint(mtu), "up")
}

// openEndpoint lets UDP to a tunnel server out of the uplink and routes it
// there, not into a tunnel.
func (c *controller) openEndpoint(a netip.Addr) error {
	if _, err := c.run("", "nft", "add", "element", "inet", "reflux", "endpoints", "{", a.String(), "}"); err != nil {
		return err
	}
	return c.ip("route", "replace", a.String()+"/32", "via", c.gw.String(), "dev", uplink)
}

func (c *controller) closeEndpoint(a netip.Addr) {
	if !c.ruDirect && a == c.ru.Endpoint.Addr() {
		return
	}
	c.run("", "nft", "delete", "element", "inet", "reflux", "endpoints", "{", a.String(), "}")
	c.ip("route", "del", a.String()+"/32", "via", c.gw.String(), "dev", uplink)
}

// failover moves the world tunnel to the next config in order.
func (c *controller) failover() error {
	next := (c.cur + 1) % len(c.world)
	log.Printf("world: %s failed, switching to %s", c.world[c.cur].Name, c.world[next].Name)
	return c.useWorld(next)
}

// applySelection switches to the world server the owner chose, once per
// choice: after a failover away from it, it is not forced back. It
// reports whether the tunnel moved.
func (c *controller) applySelection() (bool, error) {
	sel := c.selection()
	if sel == c.picked {
		return false, nil
	}
	c.picked = sel
	i := c.worldIndex(sel)
	if i < 0 || i == c.cur {
		return false, nil
	}
	log.Printf("world: %s chosen, switching from %s", sel, c.world[c.cur].Name)
	return true, c.useWorld(i)
}

// useWorld moves the world tunnel to config next. The old server leaves
// the kill switch, so at most two connections are ever open.
func (c *controller) useWorld(next int) error {
	old := c.world[c.cur]
	c.cur = next
	n := c.world[c.cur]
	if old.Endpoint.Addr() != n.Endpoint.Addr() {
		c.closeEndpoint(old.Endpoint.Addr())
	}
	if err := c.up(worldIface, n); err != nil {
		return err
	}
	// up flushes the interface's address, and the kernel drops every route
	// through an interface that loses its last IPv4 address: the default
	// route has to be put back.
	return c.ip("route", "replace", "default", "dev", worldIface, "metric", "0")
}

// setPrefixes routes the RU prefixes to Russia (the tunnel, or the uplink
// while Russia leaves directly), changing only what differs from the routes
// already there. Out of the uplink, the kill switch opens for the new
// prefixes before they are routed out, and closes for the old ones after
// they are not.
func (c *controller) setPrefixes(next []netip.Prefix, at time.Time) error {
	if len(next) < minRUPrefixes {
		return fmt.Errorf("RU list has %d prefixes, fewer than %d: not applied", len(next), minRUPrefixes)
	}
	c.routeMu.Lock()
	defer c.routeMu.Unlock()
	c.mu.Lock()
	prev := c.prefixes
	c.mu.Unlock()
	add, del := diffPrefixes(prev, next)
	if c.onDirect && len(add) > 0 {
		if _, err := c.run(setBatch("add", "ru4", add), "nft", "-f", "-"); err != nil {
			return err
		}
	}
	if len(add)+len(del) > 0 {
		if _, err := c.run(routeBatch(add, del, c.ruTarget()), "ip", "-batch", "-"); err != nil {
			return err
		}
	}
	if c.onDirect && len(del) > 0 {
		if _, err := c.run(setBatch("delete", "ru4", del), "nft", "-f", "-"); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.prefixes = next
	c.st.RUPrefixes = len(next)
	c.st.RUListAt = at
	c.mu.Unlock()
	log.Printf("ru: %d prefixes routed (+%d -%d)", len(next), len(add), len(del))
	return nil
}

// switchRussia moves the RU prefixes out of the uplink (direct) or back
// into the Russian tunnel. Out: the kill switch opens for them first, then
// they are routed out. Back: they are routed into the tunnel first, then
// the kill switch closes, so no packet to Russia is dropped on the way.
func (c *controller) switchRussia(direct bool) error {
	c.routeMu.Lock()
	defer c.routeMu.Unlock()
	c.mu.Lock()
	ps := c.prefixes
	c.mu.Unlock()
	if direct {
		if len(ps) > 0 {
			if _, err := c.run(setBatch("add", "ru4", ps), "nft", "-f", "-"); err != nil {
				return err
			}
		}
		c.onDirect = true
	} else {
		c.onDirect = false
	}
	if len(ps) > 0 {
		if _, err := c.run(routeBatch(ps, nil, c.ruTarget()), "ip", "-batch", "-"); err != nil {
			return err
		}
	}
	if !direct {
		if _, err := c.run("", "nft", "flush", "set", "inet", "reflux", "ru4"); err != nil {
			return err
		}
	}
	return nil
}

// ruRounds counts the probe rounds of Russia.
type ruRounds struct {
	failures int // in a row the way in use failed (the tunnel, or the uplink)
	healthy  int // in a row the tunnel answered
	direct   int // in a row the uplink failed while it is the fallback
}

// russia probes Russia once and, with a direct fallback, moves the RU
// prefixes out of the uplink when the tunnel has failed failLimit rounds
// and back when it has answered recoverRounds. It returns whether Russian
// traffic has a way out.
func (c *controller) russia(r *ruRounds) (bool, error) {
	if c.ruDirect {
		if c.probe(uplink, ruProbes, nil) {
			r.failures = 0
		} else {
			r.failures++
		}
		return r.failures < failLimit, nil
	}
	// ping -I binds to the interface: the tunnel is probed even while the
	// probe address is routed out of the uplink.
	if c.probe(ruIface, ruProbes, nil) {
		r.failures, r.healthy = 0, r.healthy+1
	} else {
		r.failures, r.healthy = r.failures+1, 0
	}
	tunnelUp := r.failures < failLimit
	if !c.ruFallback {
		return tunnelUp, nil
	}
	switch {
	case !c.onDirect && !tunnelUp:
		log.Printf("ru: tunnel %s does not answer: Russia leaves through the uplink until it does", c.ru.Name)
		r.direct = 0
		if err := c.switchRussia(true); err != nil {
			return false, fmt.Errorf("ru: fallback to the uplink: %w", err)
		}
	case c.onDirect && r.healthy >= recoverRounds:
		log.Printf("ru: tunnel %s answers again: Russia goes back into it", c.ru.Name)
		if err := c.switchRussia(false); err != nil {
			return false, fmt.Errorf("ru: back into the tunnel: %w", err)
		}
	}
	if !c.onDirect {
		return tunnelUp, nil
	}
	if c.probe(uplink, ruProbes, nil) {
		r.direct = 0
	} else {
		r.direct++
	}
	return r.direct < failLimit, nil
}

// probe reports whether any target answers a ping through iface, or any
// tcpTargets accepts a connection on port 443 (routed through iface).
func (c *controller) probe(iface string, targets, tcpTargets []string) bool {
	for _, t := range targets {
		if _, err := c.run("", "ping", "-c", "1", "-W", "3", "-I", iface, t); err == nil {
			return true
		}
	}
	for _, t := range tcpTargets {
		if _, err := c.run("", "nc", "-z", "-w", "3", t, "443"); err == nil {
			return true
		}
	}
	return false
}

// watch probes both tunnels forever and fails the world tunnel over. It
// never returns to an earlier config on its own: each switch breaks the
// clients' connections, so a working tunnel is kept.
func (c *controller) watch(stop <-chan struct{}) {
	failures := 0
	var ru ruRounds
	c.mu.Lock()
	c.st.World, c.st.WorldSince = c.world[c.cur].Name, time.Now().UTC()
	c.st.RUOK, c.st.RUMode = true, ruModeName(c.onDirect)
	c.mu.Unlock()
	for {
		var errText string
		if moved, err := c.applySelection(); err != nil {
			errText = "world: " + err.Error()
			log.Print(errText)
		} else if moved {
			failures = 0
			c.mu.Lock()
			c.st.World, c.st.WorldSince = c.world[c.cur].Name, time.Now().UTC()
			c.mu.Unlock()
		}
		worldOK := c.probe(worldIface, worldProbes, worldTCP)
		if worldOK {
			failures = 0
		} else {
			failures++
		}
		// One lost ping is not an outage: Russia is reported down, like the
		// world tunnel is failed over, after failLimit rounds.
		ruOK, ruErr := c.russia(&ru)
		if ruErr != nil {
			errText = ruErr.Error()
			log.Print(errText)
		}
		if failures >= failLimit && len(c.world) > 1 {
			if err := c.failover(); err != nil {
				errText = err.Error()
				log.Printf("world: failover: %v", err)
			}
			failures = 0
			c.mu.Lock()
			c.st.World, c.st.WorldSince = c.world[c.cur].Name, time.Now().UTC()
			c.mu.Unlock()
		}
		dropped := c.dropped()
		c.mu.Lock()
		if dropped >= 0 {
			c.st.Dropped = dropped
		}
		mode := ruModeName(c.onDirect)
		if c.st.RUOK != ruOK || c.st.RUMode != mode {
			log.Printf("ru: %s %s", mode, map[bool]string{true: "up", false: "down"}[ruOK])
		}
		c.st.WorldOK, c.st.RUOK, c.st.Failures, c.st.Error = worldOK, ruOK, failures, errText
		c.st.RUMode, c.st.RUFallback = mode, c.onDirect && !c.ruDirect
		c.st.Selected = c.picked
		c.st.Updated = time.Now().UTC()
		st := c.st
		c.mu.Unlock()
		if err := writeStatus(filepath.Join(c.runDir, "status.json"), st); err != nil {
			log.Printf("status: %v", err)
		}
		select {
		case <-stop:
			return
		case <-time.After(probeEvery):
		}
	}
}

func ruModeName(direct bool) string {
	if direct {
		return "direct"
	}
	return "tunnel"
}

var droppedRe = regexp.MustCompile(`oifname "` + uplink + `" counter packets (\d+) bytes \d+ drop`)

// dropped is how many packets the kill switch has stopped, or -1.
func (c *controller) dropped() int64 {
	out, err := c.run("", "nft", "list", "chain", "inet", "reflux", "output")
	if err != nil {
		return -1
	}
	m := droppedRe.FindStringSubmatch(out)
	if m == nil {
		return -1
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func writeStatus(path string, st status) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readStatus(path string) (status, error) {
	var st status
	b, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(b, &st)
	return st, err
}

// healthy is the Docker healthcheck: the world tunnel answered in the last
// probe, and the probe loop is alive. A dead Russian tunnel alone does not
// make the egress unhealthy: it only stops Russian traffic, as intended.
func healthy(st status, now time.Time) error {
	switch {
	case now.Sub(st.Updated) > 3*probeEvery+10*time.Second:
		return errors.New("probe loop stalled")
	case !st.WorldOK:
		return fmt.Errorf("world tunnel %s does not answer", st.World)
	}
	return nil
}

// statusText renders a status for people.
func statusText(st status, now time.Time) string {
	var b bytes.Buffer
	yes := map[bool]string{true: "up", false: "DOWN"}
	fmt.Fprintf(&b, "world  %-5s via %s (since %s)\n", yes[st.WorldOK], st.World, st.WorldSince.Format(time.DateTime))
	mode := st.RUMode
	if st.RUFallback {
		mode += " (fallback: the tunnel does not answer)"
	}
	fmt.Fprintf(&b, "russia %-5s %s, %d prefixes (list from %s)\n", yes[st.RUOK], mode, st.RUPrefixes, st.RUListAt.Format(time.DateOnly))
	fmt.Fprintf(&b, "kill switch stopped %d packets\n", st.Dropped)
	fmt.Fprintf(&b, "checked %s ago\n", now.Sub(st.Updated).Round(time.Second))
	if st.Error != "" {
		fmt.Fprintf(&b, "error: %s\n", st.Error)
	}
	return b.String()
}
