// Package manager coordinates a Session with several transports and exposes
// a single facade for the CLI and the mobile bridge.
//
// The Session holds the handshake state; the manager holds the transports.
// When the client asks the exit to bring up a new transport (via a
// SubtypeTransportStart control packet), the manager uses the Factory to
// build it and adds it to the Session on the fly.
package manager

import (
	"errors"
	"fmt"
	"sync"

	"openflux/transport"
	"openflux/transport/control"
	"openflux/utils"
)

// Factory builds a raw transport from a control.TransportConfig.
// main.go provides the concrete implementation because it is the only place
// that knows about every transport package (yandex, mailru, direct, ...).
type Factory func(cfg *control.TransportConfig) (transport.Transport, error)

// CookieProvider is implemented by raw transports that carry cookies.
// Manager delegates per-transport cookie operations to it.
type CookieProvider interface {
	FetchCookies() (map[string]string, error)
	ApplyCookies(jar map[string]string) error
}

// Entry is one transport attached to the manager.
type Entry struct {
	Name     string
	Type     string
	Priority int
	Raw      transport.Transport
	Provider CookieProvider // nil if the transport does not carry cookies
}

// Manager owns a Session and the transports attached to it.
type Manager struct {
	session *transport.Session
	factory Factory
	secret  string
	context string

	mu      sync.RWMutex
	entries map[string]*Entry
	order   []string // transport names, priority-descending

	// Callbacks wired from main.go / mobile bridge.
	dataCallback    func([]byte)
	controlCallback func(sub control.Subtype, payload []byte)
	captchaNotifier CaptchaNotifier
}

// New creates a Manager around a fresh Session. secret and context are the
// same values used by Session.AddTransport for bootstrap transports; they
// are reused for dynamically started ones.
func New(session *transport.Session, factory Factory, secret, context string) *Manager {
	return &Manager{
		session: session,
		factory: factory,
		secret:  secret,
		context: context,
		entries: make(map[string]*Entry),
	}
}

// Session returns the underlying Session.
func (m *Manager) Session() *transport.Session { return m.session }

// Add attaches an already-built transport. Priority is used to order
// transports for handshake and for control traffic.
func (m *Manager) Add(name, typ string, raw transport.Transport, priority int, provider CookieProvider) error {
	if name == "" {
		return errors.New("manager: empty name")
	}
	if raw == nil {
		return errors.New("manager: nil transport")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.entries[name]; exists {
		return fmt.Errorf("manager: transport %q already attached", name)
	}
	m.entries[name] = &Entry{
		Name:     name,
		Type:     typ,
		Priority: priority,
		Raw:      raw,
		Provider: provider,
	}
	m.order = insertByPriority(m.order, name, priority, m.entries)

	// If the raw transport wants to report out-of-band errors (captcha,
	// login required), subscribe on its behalf. The callback captures the
	// transport name so the notifier can route it back to the right IPC
	// channel.
	if en, ok := raw.(transport.ErrorNotifier); ok {
		transportName := name
		en.SetErrorNotifier(func(_ error, _, url, reason string) {
			m.NotifyCaptcha(transportName, url, reason)
		})
	}
	return nil
}

// Remove detaches a transport from the Session and stops it.
func (m *Manager) Remove(name string) error {
	m.mu.Lock()
	e, ok := m.entries[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("manager: transport %q not found", name)
	}
	delete(m.entries, name)
	for i, n := range m.order {
		if n == name {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.mu.Unlock()

	_ = m.session.RemoveTransport(name)
	_ = e.Raw.Stop()
	return nil
}

// Transports returns the names of attached transports in priority order.
func (m *Manager) Transports() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.order...)
}

// Start brings up the Session. The Session itself iterates transports by
// priority and performs the handshake through the first live one; the
// Manager only needs to have already registered every transport in it.
func (m *Manager) Start() error {
	m.mu.RLock()
	entries := make([]*Entry, 0, len(m.order))
	for _, name := range m.order {
		entries = append(entries, m.entries[name])
	}
	m.mu.RUnlock()

	if len(entries) == 0 {
		return errors.New("manager: no transports attached")
	}

	// main.go is expected to have already called Session.AddTransport for
	// each entry with the correct secret and context. Manager.Start() only
	// kicks off the handshake and hands the ready session back.
	return m.session.Start()
}

// Stop tears down the Session and every transport.
func (m *Manager) Stop() error {
	m.mu.Lock()
	entries := make([]*Entry, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	m.mu.Unlock()

	err := m.session.Stop()
	for _, e := range entries {
		_ = e.Raw.Stop()
	}
	return err
}

// Send routes one IPv4 packet.
func (m *Manager) Send(pkt []byte) error { return m.session.Send(pkt) }

// Receive installs the IPv4 data callback.
func (m *Manager) Receive(cb func([]byte)) {
	m.mu.Lock()
	m.dataCallback = cb
	m.mu.Unlock()
	m.session.Receive(cb)
}

// SendControl sends a control packet through the Session.
func (m *Manager) SendControl(sub control.Subtype, payload []byte) error {
	return m.session.SendControl(sub, payload)
}

// ---- CookieExchanger (per-transport) ----

// FetchCookiesFor returns the cookie jar of one transport.
func (m *Manager) FetchCookiesFor(name string) (map[string]string, error) {
	m.mu.RLock()
	e, ok := m.entries[name]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("manager: transport %q not found", name)
	}
	if e.Provider == nil {
		return nil, fmt.Errorf("manager: transport %q does not carry cookies", name)
	}
	return e.Provider.FetchCookies()
}

// ApplyCookiesFor replaces the cookie jar of one transport.
func (m *Manager) ApplyCookiesFor(name string, jar map[string]string) error {
	m.mu.RLock()
	e, ok := m.entries[name]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("manager: transport %q not found", name)
	}
	if e.Provider == nil {
		return fmt.Errorf("manager: transport %q does not carry cookies", name)
	}
	return e.Provider.ApplyCookies(jar)
}

// ---- Control dispatch ----

// DispatchControl is the Session's ControlHandler. It interprets transport
// lifecycle packets locally and forwards cookie packets to the higher layer.
func (m *Manager) DispatchControl(sub control.Subtype, payload []byte) {
	switch sub {
	case control.SubtypeTransportStart:
		cfg, err := control.DecodeTransportConfig(payload)
		if err != nil {
			utils.Debugf("[MANAGER] bad TransportStart payload: %v", err)
			return
		}
		if err := m.startTransport(cfg); err != nil {
			utils.Debugf("[MANAGER] TransportStart %q: %v", cfg.Name, err)
			m.sendStatus(cfg.Name, cfg.Type, false, err)
			return
		}
		m.sendStatus(cfg.Name, cfg.Type, true, nil)

	case control.SubtypeTransportStop:
		cfg, err := control.DecodeTransportConfig(payload)
		if err != nil {
			utils.Debugf("[MANAGER] bad TransportStop payload: %v", err)
			return
		}
		if err := m.Remove(cfg.Name); err != nil {
			utils.Debugf("[MANAGER] TransportStop %q: %v", cfg.Name, err)
		}

	case control.SubtypeTransportList:
		m.sendList()

	default:
		// Cookie and other subtypes go up to the application.
		m.mu.RLock()
		cb := m.controlCallback
		m.mu.RUnlock()
		if cb != nil {
			cb(sub, payload)
		}
	}
}

// SetControlCallback installs the callback that receives control packets
// not handled locally (cookies).
func (m *Manager) SetControlCallback(cb func(sub control.Subtype, payload []byte)) {
	m.mu.Lock()
	m.controlCallback = cb
	m.mu.Unlock()
}

// startTransport builds a transport via the Factory, registers it in the
// Session, and brings it up. Used by SubtypeTransportStart.
//
// The secret and context are the same values main.go uses for bootstrap
// transports; they are stored on the Manager at construction time.
func (m *Manager) startTransport(cfg *control.TransportConfig) error {
	if cfg == nil || cfg.Name == "" {
		return errors.New("manager: empty config")
	}
	raw, err := m.factory(cfg)
	if err != nil {
		return fmt.Errorf("factory: %w", err)
	}
	priority := 50
	if v, ok := cfg.Params["priority"].(float64); ok {
		priority = int(v)
	}
	var provider CookieProvider
	if p, ok := raw.(CookieProvider); ok {
		provider = p
	}

	if err := m.session.AddTransportPostStart(cfg.Name, raw, m.secret, m.context, priority); err != nil {
		return err
	}
	if err := m.Add(cfg.Name, cfg.Type, raw, priority, provider); err != nil {
		// Roll back the Session-side registration.
		_ = m.session.RemoveTransport(cfg.Name)
		return err
	}
	return nil
}

func (m *Manager) sendStatus(name, typ string, connected bool, err error) {
	st := control.TransportStatus{Name: name, Type: typ, Connected: connected}
	if err != nil {
		st.Error = err.Error()
	}
	body, _ := (&control.TransportStatusList{Transports: []control.TransportStatus{st}}).Encode()
	_ = m.session.SendControl(control.SubtypeTransportStatus, body)
}

func (m *Manager) sendList() {
	m.mu.RLock()
	list := make([]control.TransportStatus, 0, len(m.order))
	for _, name := range m.order {
		e := m.entries[name]
		list = append(list, control.TransportStatus{
			Name:      e.Name,
			Type:      e.Type,
			Connected: e.Raw.IsConnected(),
		})
	}
	m.mu.RUnlock()
	body, _ := (&control.TransportStatusList{Transports: list}).Encode()
	_ = m.session.SendControl(control.SubtypeTransportStatus, body)
}

func insertByPriority(order []string, name string, priority int, entries map[string]*Entry) []string {
	pos := len(order)
	for i, n := range order {
		if entries[n].Priority < priority {
			pos = i
			break
		}
	}
	out := make([]string, 0, len(order)+1)
	out = append(out, order[:pos]...)
	out = append(out, name)
	out = append(out, order[pos:]...)
	return out
}

// IsConnected reports whether the Session has completed its handshake and
// at least one transport is live.
func (m *Manager) IsConnected() bool {
	return m.session.IsConnected()
}

// Stats aggregates counters across every transport.
func (m *Manager) Stats() transport.TransportStats {
	return m.session.Stats()
}

// ---- captcha notifications ----

// CaptchaNotifier is called when a transport reports that it needs fresh
// cookies (ErrCaptchaRequired or ErrLoginRequired). Wired to the IPC server
// by main.go.
type CaptchaNotifier func(transportName, url, reason string)

// SetCaptchaNotifier installs the callback.
func (m *Manager) SetCaptchaNotifier(n CaptchaNotifier) {
	m.mu.Lock()
	m.captchaNotifier = n
	m.mu.Unlock()
}

// NotifyCaptcha is called by transports (via ErrorNotifier) when they cannot
// proceed without external help.
func (m *Manager) NotifyCaptcha(name, url, reason string) {
	m.mu.RLock()
	n := m.captchaNotifier
	m.mu.RUnlock()
	if n != nil {
		n(name, url, reason)
	}
}
