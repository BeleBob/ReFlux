package control

import "encoding/json"

// CookiesPayload is the body of SubtypeCookiesRequest/Response/Offer.
//
// The map is name -> value. Domain is optional and, when set, tells the
// receiving side which host the cookies belong to; empty means "the host
// of the document this transport is currently attached to".
type CookiesPayload struct {
	// Transport names the transport the cookies belong to. Empty (older
	// peers) means the highest-priority transport that carries cookies.
	Transport string            `json:"transport,omitempty"`
	Jar       map[string]string `json:"jar,omitempty"`
	Domain    string            `json:"domain,omitempty"`
	Reason    string            `json:"reason,omitempty"`
}

// Encode serializes the payload to JSON.
func (c *CookiesPayload) Encode() ([]byte, error) {
	return json.Marshal(c)
}

// DecodeCookies parses a payload received on the wire.
func DecodeCookies(b []byte) (*CookiesPayload, error) {
	if len(b) == 0 {
		return &CookiesPayload{}, nil
	}
	var out CookiesPayload
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
