package main

import (
	"os"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/control"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// An exit given --config-offer-file (ConfigOfferFile in a .conf) tells its
// Session client the client's current configuration (control.ConfigOffer):
// the file holds {"link":"openflux://...","version":N} and is rewritten by
// whoever manages the channel (ReFlux) when its documents change. The offer
// goes out when a client completes a handshake, when the file changes, and
// every configOfferRepeat in between, in case one was lost. Classic clients
// have no control channel and get nothing.

var (
	configOfferPoll   = 2 * time.Second
	configOfferRepeat = 5 * time.Minute
)

// configOfferSession is what the offer needs of a Session.
type configOfferSession interface {
	PeerID() [32]byte
	SendControl(control.Subtype, []byte) error
}

func runConfigOffer(s configOfferSession, path string, stop <-chan struct{}) {
	var (
		offer    control.ConfigOffer
		raw      []byte
		modTime  time.Time
		peer     [32]byte
		sentVer  int64
		sentAt   time.Time
		readErrs int
	)
	poll, repeat := configOfferPoll, configOfferRepeat
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		if st, err := os.Stat(path); err == nil && !st.ModTime().Equal(modTime) {
			b, err := os.ReadFile(path)
			if err == nil {
				if o, err := control.DecodeConfigOffer(b); err == nil {
					offer, modTime = o, st.ModTime()
					raw, _ = o.Encode()
				} else if readErrs++; readErrs == 1 {
					utils.Infof("[CONFIG] %s: %v", path, err)
				}
			}
		}
		id := s.PeerID()
		switch {
		case id == [32]byte{}:
			peer = id
		case raw == nil:
		case id != peer || offer.Version != sentVer || time.Since(sentAt) >= repeat:
			if err := s.SendControl(control.SubtypeConfigOffer, raw); err != nil {
				utils.Debugf("[CONFIG] offer: %v", err)
				break
			}
			if id != peer || offer.Version != sentVer {
				utils.Debugf("[CONFIG] offered version %d to the client", offer.Version)
			}
			peer, sentVer, sentAt = id, offer.Version, time.Now()
		}
		select {
		case <-stop:
			return
		case <-tick.C:
		}
	}
}
