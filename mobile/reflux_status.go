package mobile

import "encoding/json"

// ReFlux additions for the app's home screen: a profile that leaves its
// mail.ru documents out, and the connection test.

// SessionContext is the encryption context a Session started with specsJSON
// (what StartSession takes) derives its keys from. The node derives it from
// all of the client's documents, the mail.ru ones included, so an app that
// leaves some carriers out asks it for the full list and passes it as the
// shorter list's explicit "context": the keys stay the node's.
func SessionContext(specsJSON string) (string, error) {
	_, context, _, err := parseSessionSpecs(specsJSON)
	return context, err
}

// carrierStatus is one carrier of the running Session for the app.
type carrierStatus struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Attached: connected to its document on this side. Live: the node was
	// heard on it lately, so traffic goes over it.
	Attached bool   `json:"attached"`
	Live     bool   `json:"live"`
	HeardMs  int64  `json:"heard_ms"`
	BytesIn  uint64 `json:"bytes_in"`
	BytesOut uint64 `json:"bytes_out"`
}

// CarrierStatus tells how each carrier of the running Session is doing,
// highest priority first, as a JSON array; "[]" without a Session (classic
// mode or not connected).
func CarrierStatus() string {
	route.mu.Lock()
	s, types := route.session, route.types
	route.mu.Unlock()
	out := []carrierStatus{}
	if s != nil {
		for _, l := range s.LinkStatuses() {
			typ := types[l.Name]
			if typ == "" {
				typ = l.Name
			}
			out = append(out, carrierStatus{
				Name: l.Name, Type: typ, Attached: l.Attached, Live: l.Live,
				HeardMs: l.HeardMs, BytesIn: l.BytesIn, BytesOut: l.BytesOut,
			})
		}
	}
	b, _ := json.Marshal(out)
	return string(b)
}
