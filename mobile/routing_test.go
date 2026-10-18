package mobile

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"

	"github.com/p1neappleXpress/OpenFlux/routing"
	"github.com/p1neappleXpress/OpenFlux/tunnel"
)

const testRoutingData = `reflux-routing 1
generated test
@site category-ru
s ya.ru
@site category-ads-all
s ads.example.com
@site whitelist
s sberbank.ru
@ip ru
5.255.0.0/16
@ip whitelist
84.252.149.0/24
`

func testRouter(t *testing.T, cfg routing.Config) *routing.Router {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(testRoutingData))
	w.Close()
	d, err := routing.Load(&b)
	if err != nil {
		t.Fatal(err)
	}
	return routing.NewRouter(cfg, d)
}

var (
	device   = netip.MustParseAddr("10.10.10.2")
	upstream = netip.MustParseAddr("1.1.1.1")
)

// collector keeps the packets a side received.
type collector struct {
	mu   sync.Mutex
	pkts [][]byte
	ch   chan []byte
}

func newCollector() *collector { return &collector{ch: make(chan []byte, 256)} }

func (c *collector) put(b []byte) {
	c.mu.Lock()
	c.pkts = append(c.pkts, append([]byte(nil), b...))
	c.mu.Unlock()
	select {
	case c.ch <- b:
	default:
	}
}

func (c *collector) next(t *testing.T, within time.Duration) []byte {
	t.Helper()
	select {
	case b := <-c.ch:
		return b
	case <-time.After(within):
		t.Fatal("no packet")
		return nil
	}
}

func dnsQuery(t *testing.T, id uint16, name string, typ dnsmessage.Type) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET})
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func dnsAnswer(t *testing.T, id uint16, name string, a [4]byte) []byte {
	n := dnsmessage.MustNewName(name)
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, Response: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: n, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	b.StartAnswers()
	b.AResource(dnsmessage.ResourceHeader{Name: n, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: a})
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// checksumsOK verifies a packet's IPv4 and UDP/TCP checksums.
func checksumsOK(b []byte) bool {
	ihl := int(b[0]&0x0f) * 4
	if ^sum16(b[:ihl], 0) != 0 {
		return false
	}
	l4 := b[ihl:binary.BigEndian.Uint16(b[2:4])]
	if b[9] == protoUDP && l4[6] == 0 && l4[7] == 0 {
		return true // sent without one
	}
	var pseudo [12]byte
	copy(pseudo[0:8], b[12:20])
	pseudo[9] = b[9]
	binary.BigEndian.PutUint16(pseudo[10:12], uint16(len(l4)))
	return ^sum16(l4, uint32(sum16(pseudo[:], 0))) == 0
}

func TestRoutingDNS(t *testing.T) {
	tun := newCollector()
	pr := newPacketRouter(testRouter(t, routing.Config{Mode: routing.ModeRussia, Ads: true}), upstream, tun.put)
	defer pr.close()

	// The device asks RoutingDNS: the question goes to the upstream.
	q := udpPacket(device, routingDNS, 40000, 53, dnsQuery(t, 7, "ya.ru.", dnsmessage.TypeA))
	out := pr.outbound(q)
	if out == nil {
		t.Fatal("the question did not go to the tunnel")
	}
	if p, _ := parsePacket(out); p.dst != upstream || !checksumsOK(out) {
		t.Fatalf("question to %s, checksums ok %v", p.dst, checksumsOK(out))
	}
	// The answer comes back as from RoutingDNS, and teaches the router.
	a := udpPacket(upstream, device, 53, 40000, dnsAnswer(t, 7, "ya.ru.", [4]byte{5, 255, 255, 242}))
	in := pr.inbound(a)
	if p, _ := parsePacket(in); p.src != routingDNS || !checksumsOK(in) {
		t.Fatalf("answer from %s, checksums ok %v", p.src, checksumsOK(in))
	}
	if pr.names[netip.MustParseAddr("5.255.255.242")] != "ya.ru" {
		t.Errorf("names %v", pr.names)
	}
	// An answer to a port that never asked RoutingDNS stays as it was.
	other := udpPacket(upstream, device, 53, 41000, dnsAnswer(t, 8, "x.org.", [4]byte{1, 2, 3, 4}))
	if p, _ := parsePacket(pr.inbound(other)); p.src != upstream {
		t.Error("an answer to another port was rewritten")
	}

	// An ad name gets a blocked answer at once, from RoutingDNS.
	if pr.outbound(udpPacket(device, routingDNS, 40001, 53, dnsQuery(t, 9, "x.ads.example.com.", dnsmessage.TypeA))) != nil {
		t.Fatal("an ad question went to the tunnel")
	}
	reply := tun.next(t, time.Second)
	p, _ := parsePacket(reply)
	if p.src != routingDNS || p.dport != 40001 || !checksumsOK(reply) {
		t.Fatalf("blocked reply %+v", p)
	}
	var parser dnsmessage.Parser
	h, err := parser.Start(p.payload)
	if err != nil || h.ID != 9 || !h.Response {
		t.Fatalf("reply header %+v %v", h, err)
	}
	parser.SkipAllQuestions()
	if ah, err := parser.AnswerHeader(); err != nil || ah.Type != dnsmessage.TypeA {
		t.Fatalf("answer header %+v %v", ah, err)
	}
	if ar, err := parser.AResource(); err != nil || ar.A != [4]byte{} {
		t.Fatalf("answer %v %v", ar, err)
	}
}

// phone is a device-side stack whose packets go through the router: its
// tunnel packets land in tunnel, the router's answers come back to it.
type phone struct {
	s      *stack.Stack
	ep     *tunnel.TunnelLinkEndpoint
	pr     *packetRouter
	tunnel *collector
}

func newPhone(t *testing.T, r *routing.Router) *phone {
	t.Helper()
	ph := &phone{tunnel: newCollector()}
	ph.s = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	ph.ep = tunnel.NewTunnelLinkEndpoint()
	ph.pr = newPacketRouter(r, upstream, func(b []byte) { ph.ep.InjectInbound(b) })
	ph.ep.OnOutgoing(func(b []byte) {
		b = append([]byte(nil), b...)
		if out := ph.pr.outbound(b); out != nil {
			ph.tunnel.put(out)
		}
	})
	if err := ph.s.CreateNIC(1, ph.ep); err != nil {
		t.Fatal(err)
	}
	ph.s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4(device.As4()).WithPrefix(),
	}, stack.AddressProperties{})
	ph.s.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: 1})
	t.Cleanup(func() { ph.pr.close(); ph.s.Close() })
	return ph
}

func (ph *phone) dial(addr string, timeout time.Duration) (net.Conn, error) {
	ap := netip.MustParseAddrPort(addr)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return gonet.DialContextTCP(ctx, ph.s, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(ap.Addr().As4()), Port: ap.Port()}, ipv4.ProtocolNumber)
}

// echoServer answers each line with the same line.
func echoServer(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return l.Addr().String()
}

func setDialDirect(t *testing.T, f func(network, addr string, timeout time.Duration) (net.Conn, error)) {
	old := dialDirect
	dialDirect = f
	t.Cleanup(func() { dialDirect = old })
}

func TestRoutingDirectAndTunnel(t *testing.T) {
	echo := echoServer(t)
	var dialed []string
	var mu sync.Mutex
	setDialDirect(t, func(network, addr string, timeout time.Duration) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		return net.DialTimeout(network, echo, timeout)
	})
	ph := newPhone(t, testRouter(t, routing.Config{Mode: routing.ModeRussia}))

	// A Russian address: opened from the app, through the local stack.
	c, err := ph.dial("5.255.255.242:443", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("hello\n"))
	buf := make([]byte, 6)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello\n" {
		t.Fatalf("echo %q %v", buf, err)
	}
	c.Close()
	mu.Lock()
	if len(dialed) != 1 || dialed[0] != "5.255.255.242:443" {
		t.Errorf("dialed %v", dialed)
	}
	mu.Unlock()
	if len(ph.tunnel.pkts) != 0 {
		t.Errorf("%d packets of a direct connection went to the tunnel", len(ph.tunnel.pkts))
	}

	// Anything else: the SYN goes to the tunnel untouched.
	go ph.dial("8.8.8.8:443", 2*time.Second)
	syn := ph.tunnel.next(t, 2*time.Second)
	if p, _ := parsePacket(syn); p.dst != netip.MustParseAddr("8.8.8.8") || !p.syn {
		t.Errorf("tunnel got %+v", p)
	}
}

func TestRoutingWhitelistFallsBackToTheTunnel(t *testing.T) {
	r := testRouter(t, routing.Config{Mode: routing.ModeWhitelist})
	setDialDirect(t, func(network, addr string, timeout time.Duration) (net.Conn, error) {
		return nil, errors.New("connection timed out")
	})
	ph := newPhone(t, r)
	// The device learned sberbank.ru's address from DNS.
	ph.pr.learn(dnsAnswer(t, 1, "online.sberbank.ru.", [4]byte{84, 252, 149, 51}))

	go ph.dial("84.252.149.51:443", 5*time.Second)
	// The first SYN went to the local stack; the direct dial failed, so the
	// device's next SYN goes through the tunnel.
	syn := ph.tunnel.next(t, 4*time.Second)
	if p, _ := parsePacket(syn); !p.syn || p.dst != netip.MustParseAddr("84.252.149.51") {
		t.Fatalf("tunnel got %+v", p)
	}
	if !r.Failed("online.sberbank.ru") {
		t.Error("the site was not marked failed")
	}
	// And so does every later connection to it.
	go ph.dial("84.252.149.51:443", 2*time.Second)
	if p, _ := parsePacket(ph.tunnel.next(t, 2*time.Second)); !p.syn {
		t.Errorf("tunnel got %+v", p)
	}
}

func TestRoutingWhitelistSilentSite(t *testing.T) {
	old := checkedAnswerTimeout
	checkedAnswerTimeout = 300 * time.Millisecond
	t.Cleanup(func() { checkedAnswerTimeout = old })
	// A site that takes the connection and never answers.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go io.Copy(io.Discard, c)
		}
	}()
	setDialDirect(t, func(network, addr string, timeout time.Duration) (net.Conn, error) {
		return net.DialTimeout(network, l.Addr().String(), timeout)
	})
	r := testRouter(t, routing.Config{Mode: routing.ModeWhitelist})
	ph := newPhone(t, r)
	ph.pr.learn(dnsAnswer(t, 1, "sberbank.ru.", [4]byte{84, 252, 149, 206}))
	c, err := ph.dial("84.252.149.206:443", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("client hello"))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("a silent site answered")
	}
	if !r.Failed("sberbank.ru") {
		t.Error("the silent site was not marked failed")
	}
}

func TestRoutingRefusesOtherPortsOfRoutingDNS(t *testing.T) {
	ph := newPhone(t, testRouter(t, routing.Config{Mode: routing.ModeRussia}))
	start := time.Now()
	if _, err := ph.dial(RoutingDNSAddress+":853", 3*time.Second); err == nil {
		t.Fatal("DNS over TLS to RoutingDNS connected")
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("refused only after %v", time.Since(start))
	}
}

func TestSetRouting(t *testing.T) {
	t.Cleanup(func() { SetRouting("", "", "") })
	if msg := SetRouting(`{"mode":"nope"}`, "", ""); msg == "" {
		t.Error("bad mode accepted")
	}
	if msg := SetRouting(`{"mode":"russia"}`, "", "dns.google"); msg == "" {
		t.Error("a DNS name accepted as the server")
	}
	if msg := SetRouting(`{"mode":"tunnel"}`, "", ""); msg != "" || RoutingActive() {
		t.Errorf("tunnel mode: %q active %v", msg, RoutingActive())
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "routing.dat.gz")
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(testRoutingData))
	w.Close()
	os.WriteFile(path, b.Bytes(), 0o600)
	if msg := SetRouting(`{"mode":"whitelist","ads":true}`, path, "9.9.9.9"); msg != "" || !RoutingActive() {
		t.Fatalf("whitelist: %q", msg)
	}
	if info := RoutingDataInfo(path); info == "" {
		t.Error("RoutingDataInfo")
	}
	r := newRouter()
	if r == nil || r.Decide("sberbank.ru", netip.MustParseAddr("84.252.149.1")) != routing.DirectChecked {
		t.Error("the router from the settings")
	}
	if routingUpstream() != netip.MustParseAddr("9.9.9.9") {
		t.Error("upstream")
	}
}

// fakeTunnelDialer records what went through the tunnel.
type fakeTunnelDialer struct {
	mu   sync.Mutex
	tcp  []string
	echo string
}

func (f *fakeTunnelDialer) DialTCP(address string) (net.Conn, error) {
	f.mu.Lock()
	f.tcp = append(f.tcp, address)
	f.mu.Unlock()
	return net.Dial("tcp", f.echo)
}

func TestRoutingProxyMode(t *testing.T) {
	echo := echoServer(t)
	var direct []string
	setDialDirect(t, func(network, addr string, timeout time.Duration) (net.Conn, error) {
		direct = append(direct, addr)
		if addr == "online.sberbank.ru:443" {
			return nil, errors.New("timeout")
		}
		return net.Dial(network, echo)
	})
	oldResolve := resolveDirect
	resolveDirect = func(host string) (netip.Addr, error) { return netip.MustParseAddr("84.252.149.51"), nil }
	t.Cleanup(func() { resolveDirect = oldResolve })

	tun := &fakeTunnelDialer{echo: echo}
	ru := &routerDialer{tunnel: tun, router: testRouter(t, routing.Config{Mode: routing.ModeRussia, Ads: true})}
	for _, c := range []struct {
		addr string
		via  string // "direct", "tunnel", "blocked"
	}{
		{"ya.ru:443", "direct"},
		{"5.255.1.1:80", "direct"},
		{"example.org:443", "tunnel"},
		{"x.ads.example.com:443", "blocked"},
	} {
		nd, nt := len(direct), len(tun.tcp)
		conn, err := ru.DialTCP(c.addr)
		got := "blocked"
		switch {
		case err == nil && len(direct) > nd:
			got = "direct"
		case err == nil && len(tun.tcp) > nt:
			got = "tunnel"
		}
		if conn != nil {
			conn.Close()
		}
		if got != c.via {
			t.Errorf("%s went %s, want %s", c.addr, got, c.via)
		}
	}
	r := testRouter(t, routing.Config{Mode: routing.ModeWhitelist})
	wl := &routerDialer{tunnel: tun, router: r}
	conn, err := wl.DialTCP("online.sberbank.ru:443")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if direct[len(direct)-1] != "online.sberbank.ru:443" || tun.tcp[len(tun.tcp)-1] != "online.sberbank.ru:443" || !r.Failed("online.sberbank.ru") {
		t.Errorf("whitelisted site that fails direct: direct %v tunnel %v", direct, tun.tcp)
	}
}
