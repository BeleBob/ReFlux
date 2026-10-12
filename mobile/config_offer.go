package mobile

import (
	"sync"

	"github.com/p1neappleXpress/OpenFlux/transport/control"
)

// The exit may tell the client its current configuration (control.ConfigOffer,
// sent by ReFlux exits when the channel's documents change). The core only
// keeps the newest offer; the app takes it with PendingConfigOffer, compares
// the version with the one its profile came from, and applies a newer one.

var configOffers struct {
	sync.Mutex
	pending string // the newest offer not taken yet, as JSON
	version int64  // the newest version seen by this process
}

// receiveConfigOffer is the manager's control callback on the client side.
func receiveConfigOffer(sub control.Subtype, payload []byte) {
	if sub != control.SubtypeConfigOffer {
		return
	}
	o, err := control.DecodeConfigOffer(payload)
	if err != nil {
		appendLog("[CONFIG] ignored a config offer: " + err.Error())
		return
	}
	b, err := o.Encode()
	if err != nil {
		return
	}
	configOffers.Lock()
	defer configOffers.Unlock()
	if o.Version <= configOffers.version {
		return
	}
	configOffers.version = o.Version
	configOffers.pending = string(b)
}

// PendingConfigOffer returns the newest configuration the exit offered since
// the last call, as {"link":"reflux://...","version":N} (openflux:// from an
// older server), or "" when there is none. The link parses with
// ParseShareLink once its scheme is openflux://.
func PendingConfigOffer() string {
	configOffers.Lock()
	defer configOffers.Unlock()
	p := configOffers.pending
	configOffers.pending = ""
	return p
}
