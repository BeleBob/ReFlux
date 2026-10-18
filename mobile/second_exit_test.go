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

func TestSecondExitSharesSitesAndKeepsThem(t *testing.T) {
	pr := newPacketRouter(routing.NewRouter(routing.Config{Mode: routing.ModeTunnel}, nil), upstream, func([]byte) {})
	defer pr.close()
	var primaryUp atomic.Bool
	primaryUp.Store(true)
	pr.primaryUp = primaryUp.Load
	sec := &fakeExit{}
	sec.up.Store(true)
	pr.setSecond(sec)
	// Sites the device looked up.
	pr.learn(dnsAnswer(t, 1, "a.example.", [4]byte{93, 184, 0, 1}))
	pr.learn(dnsAnswer(t, 2, "b.example.", [4]byte{93, 184, 0, 2}))
	pr.learn(dnsAnswer(t, 3, "c.example.", [4]byte{93, 184, 0, 3}))

	main := 0
	send := func(dst string, sport uint16) {
		if pr.outbound(syn(dst, sport)) != nil {
			main++
		}
	}
	send("93.184.0.1", 1000) // a: main (both exits empty)
	send("93.184.0.2", 1001) // b: second (fewer flows)
	send("93.184.0.3", 1002) // c: main again (1:1)
	if main != 2 || sec.count() != 1 {
		t.Fatalf("main %d, second %d; want 2 and 1", main, sec.count())
	}
	// More connections to b stay on the second exit, to a on the main.
	for i := uint16(0); i < 5; i++ {
		send("93.184.0.2", 2000+i)
		send("93.184.0.1", 3000+i)
	}
	if main != 7 || sec.count() != 6 {
		t.Fatalf("sites moved between exits: main %d, second %d", main, sec.count())
	}

	// DNS goes through the main exit, or the second one while it is down.
	q := udpPacket(device, routingDNS, 40000, 53, dnsQuery(t, 9, "x.example.", dnsmessage.TypeA))
	if pr.outbound(q) == nil {
		t.Error("DNS did not go through the main exit")
	}
	primaryUp.Store(false)
	before := sec.count()
	q = udpPacket(device, routingDNS, 40001, 53, dnsQuery(t, 10, "x.example.", dnsmessage.TypeA))
	if pr.outbound(q) != nil || sec.count() != before+1 {
		t.Error("DNS did not move to the second exit with the main one down")
	}
	// New sites too.
	pr.learn(dnsAnswer(t, 4, "d.example.", [4]byte{93, 184, 0, 4}))
	if pr.outbound(syn("93.184.0.4", 4000)) != nil {
		t.Error("a new site went to the dead main exit")
	}
	primaryUp.Store(true)

	// The second exit down: new sites take the main one.
	sec.up.Store(false)
	pr.learn(dnsAnswer(t, 5, "e.example.", [4]byte{93, 184, 0, 5}))
	if pr.outbound(syn("93.184.0.5", 5000)) == nil {
		t.Error("a new site went to the dead second exit")
	}
	// Detached: everything is the main exit's.
	pr.setSecond(nil)
	if pr.outbound(syn("93.184.0.2", 6000)) == nil {
		t.Error("a flow went to a detached second exit")
	}
}
