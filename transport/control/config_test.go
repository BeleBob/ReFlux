package control

import "testing"

// ReFlux servers offer reflux:// links; an exit that refused them never sent
// an offer at all.
func TestDecodeConfigOfferTakesBothLinkSchemes(t *testing.T) {
	for _, c := range []struct {
		body string
		ok   bool
	}{
		{`{"link":"reflux://v1/abc","version":3}`, true},
		{`{"link":"openflux://v1/abc","version":3}`, true},
		{`{"link":"https://example.com","version":3}`, false},
		{`{"link":"reflux://v1/abc","version":0}`, false},
		{`{"link":"","version":3}`, false},
	} {
		o, err := DecodeConfigOffer([]byte(c.body))
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok %v", c.body, err, c.ok)
		}
		if c.ok {
			b, _ := o.Encode()
			if back, err := DecodeConfigOffer(b); err != nil || back != o {
				t.Errorf("%s: round trip gave %+v, %v", c.body, back, err)
			}
		}
	}
}
