package transport

import (
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/control"
)

// A config offer reaches the client's control handler, and the exit can
// tell one client session from the next.
func TestConfigOfferReachesTheClient(t *testing.T) {
	client, exit, _, _ := linkedSessions(t, "mailru")
	got := make(chan control.ConfigOffer, 1)
	client.SetControlHandler(func(sub control.Subtype, p []byte) {
		if sub != control.SubtypeConfigOffer {
			return
		}
		if o, err := control.DecodeConfigOffer(p); err == nil {
			got <- o
		}
	})
	if id := exit.PeerID(); id != [32]byte{} {
		t.Fatal("a peer id before any handshake")
	}
	startPair(t, client, exit)
	first := exit.PeerID()
	if first == [32]byte{} {
		t.Fatal("no peer id after the handshake")
	}
	b, _ := control.ConfigOffer{Link: "openflux://x", Version: 7}.Encode()
	if err := exit.SendControl(control.SubtypeConfigOffer, b); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-got:
		if o.Link != "openflux://x" || o.Version != 7 {
			t.Errorf("offer %+v", o)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the client never got the offer")
	}
}

func TestDecodeConfigOffer(t *testing.T) {
	for _, bad := range []string{`{"link":"https://x","version":1}`, `{"link":"openflux://x"}`, `nope`} {
		if _, err := control.DecodeConfigOffer([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
