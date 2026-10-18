package routing

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Action is what happens to a connection.
type Action uint8

const (
	// Proxy: through the tunnel.
	Proxy Action = iota
	// Direct: straight out of the device, past the tunnel.
	Direct
	// DirectChecked: direct, but the site is known to work direct only
	// from the lists: when it does not answer, it is marked failed (Failed)
	// and the next connections to it go through the tunnel.
	DirectChecked
	// Block: nowhere.
	Block
)

func (a Action) String() string {
	switch a {
	case Direct:
		return "direct"
	case DirectChecked:
		return "direct?"
	case Block:
		return "block"
	}
	return "proxy"
}

// Modes.
const (
	// ModeTunnel: everything through the tunnel (only ads and the
	// person's own sites aside).
	ModeTunnel = "tunnel"
	// ModeRussia: Russian sites and addresses direct, the rest (and what
	// Russia blocks) through the tunnel.
	ModeRussia = "russia"
	// ModeWhitelist: only what the mobile internet whitelists let through
	// direct, the rest through the tunnel.
	ModeWhitelist = "whitelist"
)

// The data file's lists the modes use.
const (
	SiteRussia       = "category-ru"
	SiteGovRussia    = "category-gov-ru"
	SiteOnlyInside   = "ru-available-only-inside"
	SiteBlocked      = "ru-blocked"
	SiteAds          = "category-ads-all"
	SiteWinSpy       = "win-spy"
	SitePrivate      = "private"
	SiteWhitelist    = "whitelist"
	IPRussia         = "ru"
	IPPrivate        = "private"
	IPWhitelist      = "whitelist"
	failedFor        = 6 * time.Hour
	maxFailedEntries = 4096
)

// Config is the person's choice, as the app passes it.
type Config struct {
	Mode string `json:"mode"`
	// Ads blocks advertising and tracking sites.
	Ads bool `json:"ads,omitempty"`
	// Direct are the person's own sites that always go direct (a domain
	// covers its subdomains).
	Direct []string `json:"direct,omitempty"`
}

// ParseConfig reads the app's JSON; "" is ModeTunnel without extras.
func ParseConfig(s string) (Config, error) {
	var c Config
	if strings.TrimSpace(s) != "" {
		if err := json.Unmarshal([]byte(s), &c); err != nil {
			return c, fmt.Errorf("routing: %w", err)
		}
	}
	switch c.Mode {
	case "":
		c.Mode = ModeTunnel
	case ModeTunnel, ModeRussia, ModeWhitelist:
	default:
		return c, fmt.Errorf("routing: unknown mode %q", c.Mode)
	}
	var direct []string
	for _, d := range c.Direct {
		d = strings.Trim(strings.ToLower(strings.TrimSpace(d)), ".")
		if d != "" {
			direct = append(direct, d)
		}
	}
	c.Direct = direct
	return c, nil
}

// Active reports whether c changes anything: with ModeTunnel and nothing
// else every packet goes through the tunnel untouched.
func (c Config) Active() bool {
	return c.Mode != ModeTunnel || c.Ads || len(c.Direct) > 0
}

// Router decides.
type Router struct {
	cfg  Config
	data *Data

	adsMask, blockedMask, russiaMask, whitelistMask uint64

	mu     sync.Mutex
	failed map[string]time.Time // hosts (or addresses) that did not answer direct
	now    func() time.Time
}

// NewRouter decides by cfg over data (nil data: only the person's own
// sites and the private addresses are known).
func NewRouter(cfg Config, data *Data) *Router {
	return &Router{
		cfg:           cfg,
		data:          data,
		adsMask:       data.mask(SiteAds, SiteWinSpy),
		blockedMask:   data.mask(SiteBlocked),
		russiaMask:    data.mask(SiteRussia, SiteGovRussia, SiteOnlyInside, SitePrivate),
		whitelistMask: data.mask(SiteWhitelist),
		failed:        map[string]time.Time{},
		now:           time.Now,
	}
}

// Config is what the router decides by.
func (r *Router) Config() Config { return r.cfg }

// BlockName reports whether a DNS question for host gets a blocked answer.
func (r *Router) BlockName(host string) bool {
	return r.cfg.Ads && r.adsMask != 0 && !r.own(host) && r.data.siteMask(host)&r.adsMask != 0
}

// own reports whether host is one of the person's own direct sites.
func (r *Router) own(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, d := range r.cfg.Direct {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// Decide says what happens to a connection to addr (and host, when known:
// "" otherwise).
func (r *Router) Decide(host string, addr netip.Addr) Action {
	if addr.IsValid() && (r.data.InIP(IPPrivate, addr) || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast()) {
		return Direct
	}
	if host != "" && r.own(host) {
		return Direct
	}
	var m uint64
	if host != "" {
		m = r.data.siteMask(host)
	}
	if r.cfg.Ads && m&r.adsMask != 0 {
		return Block
	}
	switch r.cfg.Mode {
	case ModeRussia:
		switch {
		case m&r.blockedMask != 0:
			return Proxy
		case m&r.russiaMask != 0:
			return Direct
		case addr.IsValid() && r.data.InIP(IPRussia, addr):
			return Direct
		}
	case ModeWhitelist:
		if m&r.whitelistMask != 0 && addr.IsValid() && r.data.InIP(IPWhitelist, addr) && !r.Failed(host) {
			return DirectChecked
		}
	}
	return Proxy
}

// WhitelistedName reports whether host is on the whitelist by name (its
// address decides the rest), for a caller that has the name only.
func (r *Router) WhitelistedName(host string) bool {
	return r.whitelistMask != 0 && r.data.siteMask(host)&r.whitelistMask != 0
}

// MarkFailed records that host did not answer direct: for failedFor its
// connections go through the tunnel.
func (r *Router) MarkFailed(host string) {
	if host == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.failed) >= maxFailedEntries {
		now := r.now()
		for k, at := range r.failed {
			if now.Sub(at) >= failedFor {
				delete(r.failed, k)
			}
		}
		if len(r.failed) >= maxFailedEntries {
			r.failed = map[string]time.Time{}
		}
	}
	r.failed[strings.ToLower(host)] = r.now()
}

// Failed reports whether host did not answer direct lately.
func (r *Router) Failed(host string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.failed[strings.ToLower(host)]
	if ok && r.now().Sub(at) >= failedFor {
		delete(r.failed, strings.ToLower(host))
		return false
	}
	return ok
}
