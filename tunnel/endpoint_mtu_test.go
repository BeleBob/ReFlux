package tunnel

import (
	"testing"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

type fakeLimits struct {
	ready bool
	max   int
}

func (f *fakeLimits) PeerParameters() (transport.PeerParameters, bool) {
	return transport.PeerParameters{MaxPacketSize: f.max}, f.ready
}

// The stack's MTU follows the packet limit negotiated with the current
// peer, also when the stack came first: an exit's exists before any peer.
// It stayed at 1500 instead, and the Session refused every packet over a
// smaller limit.
func TestTunnelMTUFollowsTheNegotiatedLimit(t *testing.T) {
	limits := &fakeLimits{}
	ep := NewTunnelLinkEndpoint()
	ep.limits = limits
	if m := ep.MTU(); m != 1500 {
		t.Fatalf("MTU before any peer = %d, want 1500", m)
	}
	limits.ready, limits.max = true, 1400
	if m := ep.MTU(); m != 1400 {
		t.Fatalf("MTU with a 1400-byte limit = %d, want 1400", m)
	}
	limits.max = 65000
	if m := ep.MTU(); m != 1500 {
		t.Fatalf("MTU with a 65000-byte limit = %d, want 1500", m)
	}
	// A device's MTU set outright wins.
	ep.SetMTU(1280)
	limits.max = 1400
	if m := ep.MTU(); m != 1280 {
		t.Fatalf("MTU set to 1280 = %d", m)
	}
}
