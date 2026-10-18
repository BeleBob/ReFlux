package mobile

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/p1neappleXpress/OpenFlux/routing"
	"github.com/p1neappleXpress/OpenFlux/transport"
)

// fakeExit is a tunnel exit that records what it was sent.
type fakeExit struct {
	mu   sync.Mutex
	sent [][]byte
	up   atomic.Bool
}

func (f *fakeExit) Start() error                    { return nil }
func (f *fakeExit) Stop() error                     { return nil }
func (f *fakeExit) Receive(func([]byte))            {}
func (f *fakeExit) IsConnected() bool               { return f.up.Load() }
func (f *fakeExit) Stats() transport.TransportStats { return transport.TransportStats{} }
func (f *fakeExit) Send(b []byte) error {
	f.mu.Lock()
	f.sent = append(f.sent, b)
	f.mu.Unlock()
	return nil
}
func (f *fakeExit) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// syn is a device's TCP SYN to dst:443 from sport.
func syn(dst string, sport uint16) []byte {
	b := make([]byte, 40)
	b[0] = 0x45
	b[2], b[3] = 0, 40
	b[8], b[9] = 64, protoTCP
	s, d := device.As4(), netip.MustParseAddr(dst).As4()
	copy(b[12:16], s[:])
	copy(b[16:20], d[:])
	b[20], b[21] = byte(sport>>8), byte(sport)
	b[22], b[23] = 1, 187 // 443
	b[32] = 5 << 4
	b[33] = 0x02 // SYN
	fixChecksums(b, 20, protoTCP)
	return b
}

func TestExtraExitsShareSitesAndKeepThem(t *testing.T) {
	pr := newPacketRouter(routing.NewRouter(routing.Config{Mode: routing.ModeTunnel}, nil), upstream, func([]byte) {})
	defer pr.close()
	var primaryUp atomic.Bool
	primaryUp.Store(true)
	pr.primaryUp = primaryUp.Load
	x1, x2 := &fakeExit{}, &fakeExit{}
	x1.up.Store(true)
	x2.up.Store(true)
	pr.setExtra(1, x1)
	pr.setExtra(2, x2)
	for i := byte(1); i <= 6; i++ {
		pr.learn(dnsAnswer(t, uint16(i), string([]byte{'a' + i - 1})+".example.", [4]byte{93, 184, 0, i}))
	}
	main := 0
	send := func(site byte, sport uint16) {
		if pr.outbound(syn("93.184.0."+string('0'+rune(site)), sport)) != nil {
			main++
		}
	}
	// Six sites over three exits: two each, in turn.
	for i := byte(1); i <= 6; i++ {
		send(i, uint16(1000+int(i)))
	}
	if main != 2 || x1.count() != 2 || x2.count() != 2 {
		t.Fatalf("main %d, extra 1 %d, extra 2 %d; want 2 each", main, x1.count(), x2.count())
	}
	// Each site keeps its exit.
	for i := byte(1); i <= 6; i++ {
		send(i, uint16(2000+int(i)))
	}
	if main != 4 || x1.count() != 4 || x2.count() != 4 {
		t.Fatalf("sites moved: main %d, extra 1 %d, extra 2 %d", main, x1.count(), x2.count())
	}

	// DNS: the main exit, or an extra one while it is down.
	q := udpPacket(device, routingDNS, 40000, 53, dnsQuery(t, 9, "x.example.", dnsmessage.TypeA))
	if pr.outbound(q) == nil {
		t.Error("DNS did not go through the main exit")
	}
	primaryUp.Store(false)
	n1, n2 := x1.count(), x2.count()
	q = udpPacket(device, routingDNS, 40001, 53, dnsQuery(t, 10, "x.example.", dnsmessage.TypeA))
	if pr.outbound(q) != nil || x1.count()+x2.count() != n1+n2+1 {
		t.Error("DNS did not move to an extra exit with the main one down")
	}
	// A main-exit site moves while its exit is down.
	if pr.outbound(syn("93.184.0.1", 3000)) != nil {
		t.Error("a site stayed on the dead main exit")
	}
	primaryUp.Store(true)

	// An extra exit down: its sites go elsewhere, nothing new comes to it.
	x2.up.Store(false)
	before := x2.count()
	for i := byte(1); i <= 6; i++ {
		send(i, uint16(4000+int(i)))
	}
	if x2.count() != before {
		t.Error("a dead extra exit got new flows")
	}
	// Taken out: its flows are forgotten, its slot sends nowhere.
	pr.setExtra(1, nil)
	pr.setExtra(2, nil)
	n1 = x1.count()
	for i := byte(1); i <= 6; i++ {
		send(i, uint16(5000+int(i)))
	}
	if x1.count() != n1 {
		t.Error("a removed extra exit got flows")
	}
}
