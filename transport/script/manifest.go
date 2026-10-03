// Package script hosts JS-defined transports inside a per-transport goja
// runtime. A script transport is a signed .js file that implements the
// entire transport (auth flow, wire framing, reconnect/keepalive policy)
// on top of a small host API this package injects into the runtime; the Go
// side only does real I/O (HTTP, WebSocket, timers) and pumps callbacks
// into the script's own event loop. See js/template.js for the contract.
package script

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/dop251/goja"
)

// Param describes one user-supplied input a script transport needs before
// Open() can run (e.g. a board URL, a room code). It is pure declaration -
// the manager/CLI/UI use it to know what to collect from the operator, and
// the script itself decides how to use the values it's handed back in cfg.
type Param struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Type     string `json:"type"` // "url" | "text" | "secret"
	Required bool   `json:"required"`
}

// Info is the manifest a script transport returns from Transport.info().
//
// MTU/Reliable/Ordered/HalfDuplex/MinInterval are advisory only: the core
// never fragments, reorders, or rate-limits on a script transport's behalf
// unless it explicitly opts in. The default is straight passthrough - every
// native transport ported so far (mailru included) ships whatever []byte
// it's handed as a single frame, uncapped, and script transports do the same
// unless a future transport's manifest asks for something else.
type Info struct {
	Name         string
	Version      string
	CookieDomain string // base URL cookieJar.get()/set() operate against
	MTU          int    // 0 = unbounded (default; core does not fragment)
	Reliable     bool
	Ordered      bool
	HalfDuplex   bool
	MinInterval  time.Duration
	Params       []Param

	// ScopeCookiesToParentDomain: when ApplyCookies (the generic Go-side
	// CookieExchanger - see transport.go) applies externally-supplied
	// cookies, scope them to the parent domain (disk.yandex.ru ->
	// yandex.ru) instead of the exact CookieDomain host. Some doc-collab
	// flows redirect a captcha/login solve across sibling subdomains, so a
	// host-only cookie never reaches the host that actually needed it.
	// Default false: unset, existing scripts (mailru) keep exact host-only
	// scoping, unchanged.
	ScopeCookiesToParentDomain bool

	// HTTP transport pool tuning, applied once info() is parsed and before
	// open() runs (see transport.go's tuneHTTPTransport). Go's http.Transport
	// defaults (2 idle conns/host) throttle a script that fans out many
	// concurrent requests via concurrency.pool - a throughput-oriented
	// transport (Volga-style batch relay) sets these; a plain doc-transport
	// leaves them at 0 and gets the same small-pool default every script
	// had before this field existed.
	HTTPMaxConnsPerHost int
	HTTPMaxIdleConns    int
	HTTPIdleConnTimeout time.Duration
}

// rawInfo mirrors the JSON shape scripts return from info(); durations cross
// the JS boundary as plain milliseconds since goja has no Duration type.
type rawInfo struct {
	Name                       string  `json:"name"`
	Version                    string  `json:"version"`
	CookieDomain               string  `json:"cookieDomain"`
	MTU                        int     `json:"mtu"`
	Reliable                   bool    `json:"reliable"`
	Ordered                    bool    `json:"ordered"`
	HalfDuplex                 bool    `json:"halfDuplex"`
	MinIntervalMs              float64 `json:"minIntervalMs"`
	Params                     []Param `json:"params"`
	ScopeCookiesToParentDomain bool    `json:"scopeCookiesToParentDomain"`
	HTTPMaxConnsPerHost        int     `json:"httpMaxConnsPerHost"`
	HTTPMaxIdleConns           int     `json:"httpMaxIdleConns"`
	HTTPIdleConnTimeoutMs      float64 `json:"httpIdleConnTimeoutMs"`
}

func parseInfo(v goja.Value) (Info, error) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return Info{}, fmt.Errorf("Transport.info() returned nothing")
	}
	// Round-trip through JSON instead of walking goja.Object by hand: info()
	// is a plain data object, and every field it can legally carry (string,
	// number, bool, array of objects) survives the round-trip intact.
	b, err := json.Marshal(v.Export())
	if err != nil {
		return Info{}, fmt.Errorf("Transport.info(): export: %w", err)
	}
	var raw rawInfo
	if err := json.Unmarshal(b, &raw); err != nil {
		return Info{}, fmt.Errorf("Transport.info(): decode: %w", err)
	}
	if raw.Name == "" {
		return Info{}, fmt.Errorf("Transport.info() must set `name`")
	}
	return Info{
		Name:                       raw.Name,
		Version:                    raw.Version,
		CookieDomain:               raw.CookieDomain,
		MTU:                        raw.MTU,
		Reliable:                   raw.Reliable,
		Ordered:                    raw.Ordered,
		HalfDuplex:                 raw.HalfDuplex,
		MinInterval:                time.Duration(raw.MinIntervalMs * float64(time.Millisecond)),
		Params:                     raw.Params,
		ScopeCookiesToParentDomain: raw.ScopeCookiesToParentDomain,
		HTTPMaxConnsPerHost:        raw.HTTPMaxConnsPerHost,
		HTTPMaxIdleConns:           raw.HTTPMaxIdleConns,
		HTTPIdleConnTimeout:        time.Duration(raw.HTTPIdleConnTimeoutMs * float64(time.Millisecond)),
	}, nil
}
