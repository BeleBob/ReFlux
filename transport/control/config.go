package control

import (
	"encoding/json"
	"errors"
	"strings"
)

// SubtypeConfigOffer (exit -> client) carries the client's current
// configuration as an openflux:// link with a version (ReFlux): when the
// owner gives the channel new documents, a client reached over one that
// still works learns the rest without anyone sending it a new link. Peers
// that do not know it ignore it, like any unknown subtype.
const SubtypeConfigOffer Subtype = 0x30

// ConfigOffer is the body of SubtypeConfigOffer.
type ConfigOffer struct {
	Link string `json:"link"`
	// Version grows with every change; a client applies an offer only
	// when it is newer than the one it has.
	Version int64 `json:"version"`
}

// Encode serializes the offer to JSON.
func (o ConfigOffer) Encode() ([]byte, error) { return json.Marshal(o) }

// DecodeConfigOffer parses an offer and checks it carries a link.
func DecodeConfigOffer(b []byte) (ConfigOffer, error) {
	var o ConfigOffer
	if err := json.Unmarshal(b, &o); err != nil {
		return o, err
	}
	if !strings.HasPrefix(o.Link, "openflux://") || o.Version <= 0 {
		return o, errors.New("control: a config offer needs an openflux:// link and a version")
	}
	return o, nil
}
