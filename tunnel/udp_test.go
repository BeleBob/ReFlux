package tunnel

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"openflux/transport"
)

type pairedTransport struct {
	mu   sync.RWMutex
	peer *pairedTransport
	cb   func([]byte)
}

func newTransportPair() (*pairedTransport, *pairedTransport) {
	a, b := &pairedTransport{}, &pairedTransport{}
	a.peer, b.peer = b, a
	return a, b
}

func (p *pairedTransport) Start() error { return nil }
func (p *pairedTransport) Stop() error  { return nil }
func (p *pairedTransport) Send(data []byte) error {
	p.peer.mu.RLock()
	cb := p.peer.cb
	p.peer.mu.RUnlock()
	if cb != nil {
		cb(append([]byte(nil), data...))
	}
	return nil
}
func (p *pairedTransport) Receive(cb func([]byte)) {
	p.mu.Lock()
	p.cb = cb
	p.mu.Unlock()
}
func (p *pairedTransport) IsConnected() bool               { return true }
func (p *pairedTransport) Stats() transport.TransportStats { return transport.TransportStats{} }

func TestL4UDPDatagramRoundTrip(t *testing.T) {
	localIP := testLANIPv4(t)

	echo, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteToUDP(buf[:n], addr)
		}
	}()

	clientTransport, exitTransport := newTransportPair()
	exit := NewTCPTunnelMode(exitTransport, true, ExitModeL4)
	client := NewTCPTunnelMode(clientTransport, false, ExitModeL4)
	defer exit.Close()
	defer client.Close()

	dest := net.JoinHostPort(localIP.String(), fmt.Sprintf("%d", echo.LocalAddr().(*net.UDPAddr).Port))
	conn, err := client.DialUDP(dest)
	if err != nil {
		t.Fatalf("DialUDP: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	want := []byte("openflux-udp")
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 128)
	n, err := conn.Read(got)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got[:n]) != string(want) {
		t.Fatalf("got %q, want %q", got[:n], want)
	}
}

func testLANIPv4(t *testing.T) net.IP {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("interfaces: %v", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&(net.FlagUp|net.FlagLoopback|net.FlagPointToPoint) != net.FlagUp {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err == nil && ip.To4() != nil && !ip.IsLinkLocalUnicast() {
				return ip.To4()
			}
		}
	}
	t.Skip("no non-loopback IPv4 interface for UDP integration test")
	return nil
}
