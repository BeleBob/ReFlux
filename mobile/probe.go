package mobile

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ProbeTunnel fetches target through the tunnel and the exit (the side stack
// the captcha proxy uses) and tells how it went: {"ok":true,"ms":N,"status":C}
// or {"ok":false,"error":...}; "unavailable":true when there is no Session
// tunnel to probe with, which says nothing about the tunnel. An attached carrier and a live Session say the
// channel is up; this says the internet behind it answers, so the app can
// tell a tunnel that silently stopped carrying traffic. Session mode only;
// the app excludes itself from the VPN, so its own requests would not go
// through the tunnel.
func ProbeTunnel(target string, timeoutMs int) string {
	currentAuthProxy.mu.Lock()
	p := currentAuthProxy.p
	currentAuthProxy.mu.Unlock()
	if p == nil {
		// Not a failure of the tunnel: there is nothing to probe with
		// (classic mode, or not connected yet).
		return failure(errors.New("no Session tunnel to probe"), map[string]interface{}{"unavailable": true})
	}
	addr, err := p.Addr()
	if err != nil {
		return failure(err, nil)
	}
	if addr == "" {
		return failure(errors.New("the tunnel is closing"), nil)
	}
	timeout := time.Duration(timeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: addr}), DisableKeepAlives: true},
		// A redirect is an answer too: the internet is there.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	start := time.Now()
	resp, err := client.Get(target)
	if err != nil {
		return failure(err, nil)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return failure(fmt.Errorf("HTTP %d", resp.StatusCode), map[string]interface{}{"status": resp.StatusCode})
	}
	return result(map[string]interface{}{"ms": time.Since(start).Milliseconds(), "status": resp.StatusCode})
}
