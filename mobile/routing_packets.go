package mobile

import (
	"encoding/binary"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/p1neappleXpress/OpenFlux/routing"
	"github.com/p1neappleXpress/OpenFlux/transport"
)

// packetRouter sits between the TUN and the tunnel (see routing.go).
type packetRouter struct {
	router   *routing.Router
	upstream netip.Addr // the DNS server RoutingDNS's questions go to
	toTUN    func([]byte)
	local    *localStack

	mu    sync.Mutex
	flows map[flowKey]*flow
	names map[netip.Addr]string // address -> the site a DNS answer gave it for
	// natted are the device's ports talking to RoutingDNS through the
	// upstream, by protocol, with when they last did.
	natted map[natKey]time.Time
	stop   chan struct{}
	now    func() time.Time

	// A second exit (second_exit.go): tunnel flows are shared between the
	// main exit (0, whatever Send's caller sends to) and this one (1), a
	// site always through the same one.
	second    transport.Transport
	primaryUp func() bool
	exitFlows [2]int
	sticky    map[string]stickyExit
}

type stickyExit struct {
	exit int
	at   time.Time
}

type flowKey struct {
	src, dst     netip.Addr
	sport, dport uint16
	proto        uint8
}

type flow struct {
	action routing.Action
	host   string
	last   time.Time
	exit   int // the tunnel exit of a Proxy flow
}

type natKey struct {
	proto uint8
	port  uint16
}

const (
	protoTCP = 6
	protoUDP = 17

	maxFlows  = 1 << 15
	maxNames  = 1 << 14
	maxSticky = 1 << 12
	// stickyFor: a site keeps its exit while used this recently, so it
	// does not see the address change in the middle of a session.
	stickyFor    = 30 * time.Minute
	flowIdle     = 5 * time.Minute
	natIdle      = 2 * time.Minute
	sweepEvery   = 30 * time.Second
	blockedTTL   = 60
	dnsPort      = 53
	ipHeaderSize = 20
)

func newPacketRouter(r *routing.Router, upstream netip.Addr, toTUN func([]byte)) *packetRouter {
	pr := &packetRouter{
		router:    r,
		upstream:  upstream,
		toTUN:     toTUN,
		flows:     map[flowKey]*flow{},
		names:     map[netip.Addr]string{},
		natted:    map[natKey]time.Time{},
		stop:      make(chan struct{}),
		now:       time.Now,
		sticky:    map[string]stickyExit{},
		primaryUp: func() bool { return true },
	}
	pr.local = newLocalStack(pr)
	go pr.sweep()
	return pr
}

func (pr *packetRouter) close() {
	select {
	case <-pr.stop:
		return
	default:
	}
	close(pr.stop)
	pr.local.close()
}

// packet is the parsed part of an IPv4 packet the router looks at.
type packet struct {
	ihl          int
	proto        uint8
	src, dst     netip.Addr
	sport, dport uint16
	syn, ack     bool
	payload      []byte // after the TCP/UDP header
	fragment     bool   // not the first fragment: no ports
}

func parsePacket(b []byte) (packet, bool) {
	var p packet
	if len(b) < ipHeaderSize || b[0]>>4 != 4 {
		return p, false
	}
	p.ihl = int(b[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(b[2:4]))
	if p.ihl < ipHeaderSize || total < p.ihl || total > len(b) {
		return p, false
	}
	b = b[:total]
	p.proto = b[9]
	p.src = netip.AddrFrom4([4]byte(b[12:16]))
	p.dst = netip.AddrFrom4([4]byte(b[16:20]))
	if binary.BigEndian.Uint16(b[6:8])&0x1fff != 0 {
		p.fragment = true
		return p, true
	}
	l4 := b[p.ihl:]
	switch p.proto {
	case protoTCP:
		if len(l4) < 20 {
			return p, false
		}
		off := int(l4[12]>>4) * 4
		if off < 20 || off > len(l4) {
			return p, false
		}
		p.syn, p.ack = l4[13]&0x02 != 0, l4[13]&0x10 != 0
		p.payload = l4[off:]
	case protoUDP:
		if len(l4) < 8 {
			return p, false
		}
		p.payload = l4[8:]
	default:
		return p, true
	}
	p.sport = binary.BigEndian.Uint16(l4[0:2])
	p.dport = binary.BigEndian.Uint16(l4[2:4])
	return p, true
}

// outbound takes a packet the device sends. It returns the packet for the
// tunnel (maybe rewritten), or nil when the router took it.
func (pr *packetRouter) outbound(b []byte) []byte {
	p, ok := parsePacket(b)
	if !ok || p.fragment || (p.proto != protoTCP && p.proto != protoUDP) {
		return b
	}
	if p.dst == routingDNS {
		if p.dport != dnsPort {
			// Private DNS (DoT) and the like: refuse at once, the device
			// then asks over plain DNS.
			pr.local.inject(b, routing.Block)
			return nil
		}
		if p.proto == protoUDP {
			if reply := pr.blockedReply(b, p); reply != nil {
				pr.toTUN(reply)
				return nil
			}
		}
		pr.mu.Lock()
		pr.natted[natKey{p.proto, p.sport}] = pr.now()
		pr.mu.Unlock()
		setAddr(b, p, 16, pr.upstream)
		return pr.viaAnyExit(b)
	}
	if p.dport == dnsPort {
		if p.proto == protoUDP {
			if reply := pr.blockedReply(b, p); reply != nil {
				pr.toTUN(reply)
				return nil
			}
		}
		return pr.viaAnyExit(b) // other DNS servers: through the tunnel
	}
	k := flowKey{p.src, p.dst, p.sport, p.dport, p.proto}
	now := pr.now()
	pr.mu.Lock()
	f := pr.flows[k]
	if f == nil {
		if p.proto == protoTCP && (!p.syn || p.ack) {
			// A connection that began before the router: the tunnel's.
			pr.mu.Unlock()
			return b
		}
		host := pr.names[p.dst]
		a := pr.router.Decide(host, p.dst)
		if a == routing.DirectChecked && p.proto == protoUDP {
			// QUIC to a whitelisted site cannot be checked as TCP is:
			// the tunnel, and the browser's TCP goes direct.
			a = routing.Proxy
		}
		if len(pr.flows) >= maxFlows {
			pr.dropOldest()
		}
		f = &flow{action: a, host: host}
		if a == routing.Proxy {
			f.exit = pr.pickExit(host, p.dst, now)
			pr.exitFlows[f.exit]++
		}
		pr.flows[k] = f
	}
	f.last = now
	a, exit := f.action, f.exit
	second := pr.second
	pr.mu.Unlock()
	if a == routing.Proxy {
		if exit == 1 && second != nil {
			second.Send(b)
			return nil
		}
		return b
	}
	pr.local.inject(b, a)
	return nil
}

// inbound looks at a packet from the tunnel before it goes to the device:
// it learns sites from DNS answers and puts RoutingDNS back as their source.
func (pr *packetRouter) inbound(b []byte) []byte {
	p, ok := parsePacket(b)
	if !ok || p.fragment || p.sport != dnsPort {
		return b
	}
	if p.proto == protoUDP {
		pr.learn(p.payload)
	}
	if p.src == pr.upstream && (p.proto == protoUDP || p.proto == protoTCP) {
		pr.mu.Lock()
		_, mine := pr.natted[natKey{p.proto, p.dport}]
		pr.mu.Unlock()
		if mine {
			setAddr(b, p, 12, routingDNS)
		}
	}
	return b
}

// fromLocal takes a packet the phone's own stack sends to the device. A
// connection that moved to the tunnel (a whitelisted site that did not
// answer direct) hears nothing more from the local stack: its next SYN
// goes through the tunnel.
func (pr *packetRouter) fromLocal(b []byte) {
	p, ok := parsePacket(b)
	if ok && !p.fragment && (p.proto == protoTCP || p.proto == protoUDP) {
		pr.mu.Lock()
		f := pr.flows[flowKey{p.dst, p.src, p.dport, p.sport, p.proto}]
		moved := f != nil && f.action == routing.Proxy
		pr.mu.Unlock()
		if moved {
			return
		}
	}
	pr.toTUN(b)
}

// flowOf returns a local connection's flow (client side first).
func (pr *packetRouter) flowOf(src, dst netip.Addr, sport, dport uint16, proto uint8) (routing.Action, string, bool) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	f := pr.flows[flowKey{src, dst, sport, dport, proto}]
	if f == nil {
		return routing.Proxy, "", false
	}
	return f.action, f.host, true
}

// moveToTunnel sends a flow's next packets through the tunnel.
func (pr *packetRouter) moveToTunnel(src, dst netip.Addr, sport, dport uint16, proto uint8) {
	pr.mu.Lock()
	if f := pr.flows[flowKey{src, dst, sport, dport, proto}]; f != nil {
		if f.action != routing.Proxy {
			f.action = routing.Proxy
			f.exit = pr.pickExit(f.host, dst, pr.now())
			pr.exitFlows[f.exit]++
		}
	}
	pr.mu.Unlock()
}

// pickExit chooses the tunnel exit of a new flow to host (or, unnamed, to
// addr): the site's exit while it keeps using it, else the exit with fewer
// flows. A dead exit takes nothing new. The caller holds pr.mu.
func (pr *packetRouter) pickExit(host string, addr netip.Addr, now time.Time) int {
	if pr.second == nil || !pr.second.IsConnected() {
		return 0
	}
	if !pr.primaryUp() {
		return 1
	}
	key := host
	if key == "" {
		key = addr.String()
	}
	if c, ok := pr.sticky[key]; ok && now.Sub(c.at) < stickyFor {
		pr.sticky[key] = stickyExit{c.exit, now}
		return c.exit
	}
	e := 0
	if pr.exitFlows[1] < pr.exitFlows[0] {
		e = 1
	}
	if len(pr.sticky) >= maxSticky {
		for k, c := range pr.sticky {
			if now.Sub(c.at) >= stickyFor {
				delete(pr.sticky, k)
			}
		}
		if len(pr.sticky) >= maxSticky {
			pr.sticky = map[string]stickyExit{}
		}
	}
	pr.sticky[key] = stickyExit{e, now}
	return e
}

// viaAnyExit sends a packet that belongs to no flow (DNS) through the main
// exit, or through the second one while the main one is down.
func (pr *packetRouter) viaAnyExit(b []byte) []byte {
	pr.mu.Lock()
	second := pr.second
	pr.mu.Unlock()
	if second != nil && second.IsConnected() && !pr.primaryUp() {
		second.Send(b)
		return nil
	}
	return b
}

// forget drops a flow. The caller holds pr.mu.
func (pr *packetRouter) forget(k flowKey) {
	if f := pr.flows[k]; f != nil && f.action == routing.Proxy {
		pr.exitFlows[f.exit]--
	}
	delete(pr.flows, k)
}

// setSecond attaches (or, nil, detaches) the second exit.
func (pr *packetRouter) setSecond(t transport.Transport) {
	pr.mu.Lock()
	pr.second = t
	if t == nil {
		pr.sticky = map[string]stickyExit{}
	}
	pr.mu.Unlock()
}

func (pr *packetRouter) dropOldest() {
	var oldest flowKey
	var at time.Time
	for k, f := range pr.flows {
		if at.IsZero() || f.last.Before(at) {
			oldest, at = k, f.last
		}
	}
	pr.forget(oldest)
}

func (pr *packetRouter) sweep() {
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-pr.stop:
			return
		case <-t.C:
		}
		now := pr.now()
		pr.mu.Lock()
		for k, f := range pr.flows {
			if now.Sub(f.last) > flowIdle {
				pr.forget(k)
			}
		}
		for k, at := range pr.natted {
			if now.Sub(at) > natIdle {
				delete(pr.natted, k)
			}
		}
		pr.mu.Unlock()
	}
}

// learn records which site each address of a DNS answer is for: the
// question's name, also for the addresses of a CNAME chain.
func (pr *packetRouter) learn(msg []byte) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil || !h.Response || h.RCode != dnsmessage.RCodeSuccess {
		return
	}
	q, err := p.Question()
	if err != nil {
		return
	}
	host := strings.TrimSuffix(strings.ToLower(q.Name.String()), ".")
	if err := p.SkipAllQuestions(); err != nil {
		return
	}
	var addrs []netip.Addr
	for {
		ah, err := p.AnswerHeader()
		if err != nil {
			break
		}
		if ah.Type != dnsmessage.TypeA {
			if p.SkipAnswer() != nil {
				break
			}
			continue
		}
		a, err := p.AResource()
		if err != nil {
			break
		}
		addrs = append(addrs, netip.AddrFrom4(a.A))
	}
	if len(addrs) == 0 || host == "" {
		return
	}
	pr.mu.Lock()
	if len(pr.names)+len(addrs) > maxNames {
		pr.names = map[netip.Addr]string{}
	}
	for _, a := range addrs {
		pr.names[a] = host
	}
	pr.mu.Unlock()
}

// blockedReply answers a DNS question for an ad site at once (A 0.0.0.0,
// AAAA ::, nothing for other types); nil when the name is not blocked.
func (pr *packetRouter) blockedReply(b []byte, p packet) []byte {
	var parser dnsmessage.Parser
	h, err := parser.Start(p.payload)
	if err != nil || h.Response {
		return nil
	}
	q, err := parser.Question()
	if err != nil {
		return nil
	}
	if !pr.router.BlockName(strings.TrimSuffix(q.Name.String(), ".")) {
		return nil
	}
	reply := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: h.ID, Response: true, RecursionDesired: h.RecursionDesired, RecursionAvailable: true,
	})
	reply.EnableCompression()
	if reply.StartQuestions() != nil || reply.Question(q) != nil || reply.StartAnswers() != nil {
		return nil
	}
	rh := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: blockedTTL}
	switch q.Type {
	case dnsmessage.TypeA:
		if reply.AResource(rh, dnsmessage.AResource{}) != nil {
			return nil
		}
	case dnsmessage.TypeAAAA:
		if reply.AAAAResource(rh, dnsmessage.AAAAResource{}) != nil {
			return nil
		}
	}
	msg, err := reply.Finish()
	if err != nil {
		return nil
	}
	return udpPacket(p.dst, p.src, p.dport, p.sport, msg)
}

// udpPacket builds an IPv4 UDP packet.
func udpPacket(src, dst netip.Addr, sport, dport uint16, payload []byte) []byte {
	b := make([]byte, ipHeaderSize+8+len(payload))
	b[0] = 0x45
	binary.BigEndian.PutUint16(b[2:4], uint16(len(b)))
	b[8] = 64
	b[9] = protoUDP
	s, d := src.As4(), dst.As4()
	copy(b[12:16], s[:])
	copy(b[16:20], d[:])
	u := b[ipHeaderSize:]
	binary.BigEndian.PutUint16(u[0:2], sport)
	binary.BigEndian.PutUint16(u[2:4], dport)
	binary.BigEndian.PutUint16(u[4:6], uint16(8+len(payload)))
	copy(u[8:], payload)
	u[6] = 0xff // not "none": fixChecksums computes it
	fixChecksums(b, ipHeaderSize, protoUDP)
	return b
}

// setAddr rewrites the source (off 12) or destination (off 16) address of
// a TCP or UDP packet and its checksums.
func setAddr(b []byte, p packet, off int, a netip.Addr) {
	v := a.As4()
	copy(b[off:off+4], v[:])
	fixChecksums(b[:binary.BigEndian.Uint16(b[2:4])], p.ihl, p.proto)
}

// fixChecksums recomputes the IPv4 header checksum and the TCP or UDP one
// (a UDP packet sent without one keeps none).
func fixChecksums(b []byte, ihl int, proto uint8) {
	b[10], b[11] = 0, 0
	binary.BigEndian.PutUint16(b[10:12], ^sum16(b[:ihl], 0))
	l4 := b[ihl:]
	var at int
	switch proto {
	case protoTCP:
		at = 16
	case protoUDP:
		at = 6
		if l4[6] == 0 && l4[7] == 0 {
			return
		}
	default:
		return
	}
	l4[at], l4[at+1] = 0, 0
	var pseudo [12]byte
	copy(pseudo[0:8], b[12:20])
	pseudo[9] = proto
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(len(l4)))
	c := ^sum16(l4, uint32(sum16(pseudo[:], 0)))
	if proto == protoUDP && c == 0 {
		c = 0xffff
	}
	binary.BigEndian.PutUint16(l4[at:at+2], c)
}

func sum16(b []byte, s uint32) uint16 {
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		s += uint32(b[len(b)-1]) << 8
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return uint16(s)
}
