package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// The node's offer holds the client's current link; its version moves
// only when the link does, and never backwards.
func TestOfferFollowsTheLink(t *testing.T) {
	s := Store{Root: t.TempDir()}
	c, err := s.Add("phone", "mailru", testURL)
	if err != nil {
		t.Fatal(err)
	}
	read := func() offer {
		t.Helper()
		b, err := os.ReadFile(s.offerPath("phone"))
		if err != nil {
			t.Fatal(err)
		}
		var o offer
		json.Unmarshal(b, &o)
		return o
	}
	now := time.Now()
	if err := s.writeOffer(c, now); err != nil {
		t.Fatal(err)
	}
	first := read()
	key, _ := s.Key("phone")
	if want, _ := shownLink(c, key); first.Link != want || !strings.HasPrefix(want, refluxPrefix) || first.Version != now.Unix() {
		t.Errorf("first offer %+v", first)
	}
	if st, _ := os.Stat(s.offerPath("phone")); st.Mode().Perm() != 0o600 {
		t.Errorf("offer mode %v", st.Mode().Perm())
	}
	s.writeOffer(c, now.Add(time.Hour))
	if o := read(); o != first {
		t.Errorf("rewritten without a change: %+v", o)
	}
	c, _ = s.AddDoc("phone", Doc{"mailru", docB})
	s.writeOffer(c, now.Add(-time.Hour)) // a clock set back
	if o := read(); o.Link == first.Link || o.Version != first.Version+1 {
		t.Errorf("after a new document: %+v (was %+v)", o, first)
	}
	if conf := nodeConf(c); !strings.Contains(conf, "ConfigOfferFile = /config/offer\n") {
		t.Errorf("node.conf has no offer file:\n%s", conf)
	}
}
