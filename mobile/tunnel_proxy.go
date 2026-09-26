package mobile

// TunnelHTTPProxy returns a loopback HTTP proxy (host:port) whose
// connections leave through the running Session client's tunnel and the
// exit, or "" when there is none (classic mode, nothing running). The app
// keeps itself out of its own VPN, so its requests that must go through
// the tunnel (where does traffic leave?) use this proxy instead.
func TunnelHTTPProxy() string {
	currentAuthProxy.mu.Lock()
	p := currentAuthProxy.p
	currentAuthProxy.mu.Unlock()
	if p == nil {
		return ""
	}
	addr, err := p.Addr()
	if err != nil {
		appendLog("[ERROR] Прокси через туннель: " + err.Error())
		return ""
	}
	return addr
}
