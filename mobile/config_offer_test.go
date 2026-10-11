package mobile

import (
	"testing"

	"github.com/p1neappleXpress/OpenFlux/transport/control"
)

// The app gets each newer offer once; older or repeated ones are dropped.
func TestPendingConfigOffer(t *testing.T) {
	configOffers.Lock()
	configOffers.pending, configOffers.version = "", 0
	configOffers.Unlock()
	offer := func(v int64) {
		b, _ := control.ConfigOffer{Link: "openflux://x", Version: v}.Encode()
		receiveConfigOffer(control.SubtypeConfigOffer, b)
	}
	if p := PendingConfigOffer(); p != "" {
		t.Fatalf("pending %q at start", p)
	}
	offer(3)
	if p := PendingConfigOffer(); p != `{"link":"openflux://x","version":3}` {
		t.Errorf("pending %q", p)
	}
	if p := PendingConfigOffer(); p != "" {
		t.Errorf("taken twice: %q", p)
	}
	offer(3)
	offer(2)
	receiveConfigOffer(control.SubtypeConfigOffer, []byte(`{"link":"https://x","version":9}`))
	receiveConfigOffer(control.SubtypeLinkPing, nil)
	if p := PendingConfigOffer(); p != "" {
		t.Errorf("an old, repeated or bad offer kept: %q", p)
	}
	offer(4)
	if p := PendingConfigOffer(); p != `{"link":"openflux://x","version":4}` {
		t.Errorf("pending %q", p)
	}
}
