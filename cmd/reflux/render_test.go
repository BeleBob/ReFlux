package main

import (
	"strings"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/share"
)

// The links people see start with reflux://; the rest is the core's link,
// which ReFlux for Android 1.1.1+ reads by swapping the scheme back.
func TestShownLinkIsReflux(t *testing.T) {
	c := Client{Name: "phone", Transport: "mailru", URL: testURL, Backups: []Doc{{"mailru", docB}}}
	key := strings.Repeat("k", 32)
	link, err := shownLink(c, key)
	if err != nil || !strings.HasPrefix(link, "reflux://v1/") {
		t.Fatalf("link %q, %v", link, err)
	}
	cfg, err := share.Decode(share.Prefix + strings.TrimPrefix(link, refluxPrefix))
	if err != nil || cfg.Secret != key || len(cfg.Transports) != 2 {
		t.Fatalf("decoded %+v, %v", cfg, err)
	}
}
