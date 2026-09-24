package transport

import (
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"openflux/transport/control"
	"openflux/utils"
)

// Session is one logical session between a client and an exit node.
//
// A Session outlives any particular transport: it holds one handshake, one
// challenge pair, one sequence, and one replay window, while routing IPv4
// packets and control messages across one or more underlying transports.
//
// When the active transport dies, its in-flight flows are lost (TCP will
// retransmit); new flows are routed to whichever live transport is left.
// The handshake is NOT repeated on transport changes.
//
// Each transport is wrapped in its own EncryptedTransport + BatchedTransport
// inside a transportLink. The Session coordinates them.
type Session struct {
	mu sync.Mutex

	// Handshake state, shared across all transports.
	local            [32]byte
	peer             [32]byte
	params           PeerParameters
	remote           PeerParameters
	exit             bool
	ready            bool
	started          bool
	stopped          bool
	sequence         uint64
	highest          uint64
	window           uint64
	handshakeTimeout time.Duration

	// Transport links, ordered by priority (descending).
	links map[string]*transportLink
	order []string

	// Callbacks.
	dataCallback    func([]byte)
	controlCallback ControlHandler

	done chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

// transportLink wraps one raw Transport with its encryption and batching.
type transportLink struct {
	name      string
	raw       Transport
	encrypted *EncryptedTransport
	batched   *BatchedTransport
	priority  int

	// Set once the link has been observed to fail; it is removed from
	// routing but kept for stats until RemoveTransport is called.
	dead bool
}

// NewSession builds an empty Session. Use AddTransport to attach transports
// before calling Start.
//
// inner must be an *EncryptedTransport; unencrypted sessions are rejected.
func NewSession(p PeerParameters, exit bool) (*Session, error) {
	if !validParameters(p) {
		return nil, errors.New("session requires IPv4/TCP and a packet limit from 1280 to 65000")
	}
	s := &Session{
		params:           p,
		exit:             exit,
		links:            make(map[string]*transportLink),
		done:             make(chan struct{}),
		handshakeTimeout: 20 * time.Second,
	}
	if _, err := rand.Read(s.local[:]); err != nil {
		return nil, err
	}
	return s, nil
}

func validParameters(p PeerParameters) bool {
	const allowed = control.CapabilityIPv4 | control.CapabilityTCP |
		control.CapabilityUDP | control.CapabilityICMPErrors
	return p.Capabilities&^allowed == 0 &&
		p.Capabilities&(control.CapabilityIPv4|control.CapabilityTCP) ==
			control.CapabilityIPv4|control.CapabilityTCP &&
		p.MaxPacketSize >= 1280 && p.MaxPacketSize <= MaxNegotiatedPacket
}

// AddTransport wraps raw with EncryptedTransport + BatchedTransport and
// attaches it to the Session. name must be unique; priority controls the
// order in which transports are tried during handshake and selected for
// control traffic (higher = preferred).
//
// secret and context are the same values that would be passed to
// NewEncryptedTransport.
func (s *Session) AddTransport(name string, raw Transport, secret, context string, priority int) error {
	if raw == nil {
		return errors.New("session: nil transport")
	}
	if name == "" {
		return errors.New("session: empty transport name")
	}

	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("session: cannot add transport after Start")
	}
	if _, exists := s.links[name]; exists {
		s.mu.Unlock()
		return fmt.Errorf("session: transport %q already added", name)
	}
	s.mu.Unlock()

	enc, err := NewEncryptedTransport(raw, secret, context, s.exit)
	if err != nil {
		return fmt.Errorf("session: wrap %q: %w", name, err)
	}
	bat := NewBatchedTransport(enc)

	link := &transportLink{
		name:      name,
		raw:       raw,
		encrypted: enc,
		batched:   bat,
		priority:  priority,
	}

	s.mu.Lock()
	s.links[name] = link
	s.order = insertByPriority(s.order, name, link.priority, s.links)
	s.mu.Unlock()

	return nil
}

// RemoveTransport detaches a transport from the Session.
func (s *Session) RemoveTransport(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	link, ok := s.links[name]
	if !ok {
		return fmt.Errorf("session: transport %q not found", name)
	}
	delete(s.links, name)
	for i, n := range s.order {
		if n == name {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	_ = link.batched.Stop()
	return nil
}

// insertByPriority inserts name into order at the correct position.
// Caller holds s.mu.
func insertByPriority(order []string, name string, priority int, links map[string]*transportLink) []string {
	pos := len(order)
	for i, n := range order {
		if links[n].priority < priority {
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

// Start brings up the transports in priority order and performs exactly one
// handshake through the first live one. If that transport fails to start or
// the handshake times out, the next transport is tried. Once ready, all
// transports are live for IPv4 routing.
//
// Start is idempotent in the sense that calling it twice returns an error.
func (s *Session) Start() (err error) {
	s.mu.Lock()
	if s.stopped || s.started {
		s.mu.Unlock()
		return errors.New("session already started or stopped")
	}
	s.started = true
	s.wg.Add(1)
	handshakeTimeout := s.handshakeTimeout
	s.mu.Unlock()
	defer func() {
		s.wg.Done()
		if err != nil {
			_ = s.Stop()
		}
	}()

	s.mu.Lock()
	order := append([]string(nil), s.order...)
	s.mu.Unlock()

	if len(order) == 0 {
		return errors.New("session: no transports added")
	}

	var lastErr error
	for _, name := range order {
		s.mu.Lock()
		link, ok := s.links[name]
		s.mu.Unlock()
		if !ok {
			continue
		}

		if err := link.raw.Start(); err != nil {
			utils.Debugf("[SESSION] transport %q start: %v", name, err)
			lastErr = err
			continue
		}
		if err := link.batched.Start(); err != nil {
			utils.Debugf("[SESSION] transport %q batched start: %v", name, err)
			lastErr = err
			_ = link.raw.Stop()
			continue
		}

		// Attach the receive path: each transport delivers to the same
		// Session-level handler.
		link.batched.Receive(func(raw []byte) {
			s.receive(raw)
		})

		if err := s.handshakeVia(name, handshakeTimeout); err != nil {
			utils.Debugf("[SESSION] handshake via %q failed: %v", name, err)
			lastErr = err
			_ = link.batched.Stop()
			continue
		}

		utils.Debugf("[SESSION] ready via transport %q", name)
		return nil
	}

	if lastErr == nil {
		lastErr = errors.New("session: no live transport for handshake")
	}
	return fmt.Errorf("session: handshake failed: %w", lastErr)
}

// handshakeVia runs the hello exchange through one specific transport.
func (s *Session) handshakeVia(name string, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()

	for {
		if err := s.helloVia(name); err != nil {
			// Transient send error: keep trying within the budget.
			utils.Debugf("[SESSION] hello via %q: %v", name, err)
		}
		if s.IsConnected() {
			return nil
		}
		select {
		case <-s.done:
			return errors.New("session stopped")
		case <-timer.C:
			return errors.New("handshake timed out")
		case <-tick.C:
		}
	}
}

// Stop tears down all transports and marks the session stopped.
func (s *Session) Stop() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.ready = false
		close(s.done)
		links := make([]*transportLink, 0, len(s.links))
		for _, l := range s.links {
			links = append(links, l)
		}
		s.mu.Unlock()
		for _, l := range links {
			_ = l.batched.Stop()
			_ = l.raw.Stop()
		}
	})
	s.wg.Wait()
	return nil
}

// IsConnected reports whether the session has completed the handshake and
// at least one transport is live.
func (s *Session) IsConnected() bool {
	s.mu.Lock()
	ready := s.ready && !s.stopped
	s.mu.Unlock()
	if !ready {
		return false
	}
	return s.anyLive()
}

// anyLive reports whether at least one transport is currently connected.
func (s *Session) anyLive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.links {
		if !l.dead && l.raw.IsConnected() {
			return true
		}
	}
	return false
}

// PeerParameters returns the negotiated peer parameters, if ready.
func (s *Session) PeerParameters() (PeerParameters, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remote, s.ready && !s.stopped
}

// Transports returns the list of transport names currently attached,
// in priority order.
func (s *Session) Transports() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

// Receive installs the IPv4 data callback.
func (s *Session) Receive(cb func([]byte)) {
	s.mu.Lock()
	s.dataCallback = cb
	s.mu.Unlock()
}

// SetControlHandler installs the control-packet callback. Must be called
// before Start. Passing nil disables delivery.
func (s *Session) SetControlHandler(h ControlHandler) {
	s.mu.Lock()
	s.controlCallback = h
	s.mu.Unlock()
}

// ---- hello ----

func (s *Session) buildHello() *control.Envelope {
	env := &control.Envelope{
		Kind:  control.KindHello,
		Role:  s.roleLocked(),
		Local: s.local,
		Peer:  s.peer,
		Hello: &control.HelloTail{
			Capabilities:  control.Capabilities(s.params.Capabilities),
			MaxPacketSize: uint16(s.params.MaxPacketSize),
		},
	}
	if s.ready {
		env.Hello.Ready = 1
	}
	return env
}

func (s *Session) helloVia(name string) error {
	s.mu.Lock()
	link, ok := s.links[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("transport %q not found", name)
	}
	env := s.buildHello()
	s.mu.Unlock()

	raw, err := env.Encode()
	if err != nil {
		return err
	}
	return link.batched.Send(raw)
}

// ---- IPv4 data ----

// Send routes one complete IPv4 packet across one of the live transports,
// chosen by flow-hash. If the flow's transport is dead, the packet is
// retried on another live transport.
func (s *Session) Send(p []byte) error {
	s.mu.Lock()
	if !s.ready || s.stopped {
		s.mu.Unlock()
		return ErrNegotiationPending
	}
	if err := permittedPacket(p, s.remote); err != nil {
		s.mu.Unlock()
		return err
	}
	if s.sequence == ^uint64(0) {
		s.mu.Unlock()
		return errors.New("session sequence exhausted; restart both peers")
	}
	s.sequence++
	seq := s.sequence
	env := &control.Envelope{
		Kind:  control.KindIPv4,
		Role:  s.roleLocked(),
		Local: s.local,
		Peer:  s.peer,
		Data:  &control.DataTail{Sequence: seq},
	}
	links := s.liveLinksLocked()
	s.mu.Unlock()

	if len(links) == 0 {
		return errors.New("session: no live transport")
	}

	raw, err := env.Encode()
	if err != nil {
		return err
	}
	raw = append(raw, p...)

	// flow-hash: same 4-tuple always lands on the same transport, so
	// ordering within one flow is preserved.
	key := extractFlowKeyBytes(p)
	idx := int(flowHashBytes(key) % uint64(len(links)))
	return links[idx].batched.Send(raw)
}

// liveLinksLocked returns the live transports in priority order.
// Caller holds s.mu.
func (s *Session) liveLinksLocked() []*transportLink {
	out := make([]*transportLink, 0, len(s.links))
	for _, name := range s.order {
		l := s.links[name]
		if l == nil || l.dead {
			continue
		}
		if !l.raw.IsConnected() {
			continue
		}
		out = append(out, l)
	}
	return out
}

// SendControl transmits a control packet through the highest-priority live
// transport. Control is not covered by the replay window.
func (s *Session) SendControl(subtype control.Subtype, payload []byte) error {
	if subtype == 0 {
		return errors.New("session: empty control subtype")
	}
	if len(payload) > 0xffff {
		return fmt.Errorf("session: control payload too large (%d bytes)", len(payload))
	}
	s.mu.Lock()
	if !s.ready || s.stopped {
		s.mu.Unlock()
		return ErrNegotiationPending
	}
	env := &control.Envelope{
		Kind:  control.KindControl,
		Role:  s.roleLocked(),
		Local: s.local,
		Peer:  s.peer,
		Control: &control.ControlTail{
			Subtype:    subtype,
			Flags:      0,
			PayloadLen: uint16(len(payload)),
		},
	}
	var link *transportLink
	for _, name := range s.order {
		l := s.links[name]
		if l == nil || l.dead {
			continue
		}
		if !l.raw.IsConnected() {
			continue
		}
		link = l
		break
	}
	s.mu.Unlock()

	if link == nil {
		return errors.New("session: no live transport for control")
	}

	raw, err := env.Encode()
	if err != nil {
		return err
	}
	if len(payload) > 0 {
		raw = append(raw, payload...)
	}
	return link.batched.Send(raw)
}

func permittedPacket(p []byte, limits PeerParameters) error {
	if len(p) < 20 || p[0]>>4 != 4 ||
		int(p[0]&15)*4 < 20 || int(p[0]&15)*4 > len(p) {
		return errors.New("session: requires complete IPv4 packets")
	}
	if len(p) > limits.MaxPacketSize {
		return fmt.Errorf("IPv4 packet exceeds negotiated maximum %d", limits.MaxPacketSize)
	}
	switch p[9] {
	case 6:
	case 17:
		if limits.Capabilities&control.CapabilityUDP == 0 {
			return errors.New("peer does not support UDP")
		}
	case 1:
		if limits.Capabilities&control.CapabilityICMPErrors == 0 {
			return errors.New("peer does not support ICMP errors")
		}
	default:
		return errors.New("unsupported IP protocol")
	}
	return nil
}

// ---- receive ----

func (s *Session) receive(p []byte) {
	env, err := control.Decode(p)
	if err != nil {
		return
	}
	if env.Role == s.roleLocked() {
		return
	}
	switch env.Kind {
	case control.KindHello:
		s.receiveHello(p, env)
	case control.KindIPv4:
		s.receiveIPv4(p, env)
	case control.KindControl:
		s.receiveControl(p, env)
	}
}

func (s *Session) roleLocked() control.Role {
	if s.exit {
		return control.RoleExit
	}
	return control.RoleClient
}

func (s *Session) receiveHello(p []byte, env *control.Envelope) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	if env.Hello == nil || env.Hello.Reserved != 0 || env.Hello.Ready > 1 {
		s.mu.Unlock()
		return
	}
	sender := env.Local
	if sender == ([32]byte{}) {
		s.mu.Unlock()
		return
	}
	params := PeerParameters{
		Capabilities:  control.Capabilities(env.Hello.Capabilities),
		MaxPacketSize: int(env.Hello.MaxPacketSize),
	}
	if !validParameters(params) {
		s.mu.Unlock()
		return
	}
	if s.ready && (sender != s.peer ||
		params.Capabilities&s.params.Capabilities != s.remote.Capabilities ||
		minInt(params.MaxPacketSize, s.params.MaxPacketSize) != s.remote.MaxPacketSize) {
		s.mu.Unlock()
		return
	}
	echo := env.Peer == s.local
	zero := env.Peer == ([32]byte{})
	if !echo && !zero {
		s.mu.Unlock()
		return
	}
	changed := sender != s.peer
	wasReady := s.ready
	s.peer = sender
	if echo {
		s.remote = PeerParameters{
			Capabilities:  params.Capabilities & s.params.Capabilities,
			MaxPacketSize: minInt(params.MaxPacketSize, s.params.MaxPacketSize),
		}
		s.ready = true
	}
	// Reply through every live transport so the peer sees the new ready
	// state regardless of which one it is listening on.
	var names []string
	if changed || (!wasReady && echo) || (wasReady && env.Hello.Ready == 0) {
		names = append(names, s.order...)
	}
	s.mu.Unlock()

	for _, name := range names {
		_ = s.helloVia(name)
	}
}

func (s *Session) receiveIPv4(p []byte, env *control.Envelope) {
	s.mu.Lock()
	if env.Data == nil || !s.ready || s.stopped || env.Local != s.peer || env.Peer != s.local {
		s.mu.Unlock()
		return
	}
	payload := p[control.EnvelopeSize:]
	if permittedPacket(payload, s.remote) != nil {
		s.mu.Unlock()
		return
	}
	if !s.acceptSequenceLocked(env.Data.Sequence) {
		s.mu.Unlock()
		return
	}
	cb := s.dataCallback
	s.mu.Unlock()
	if cb != nil {
		cb(append([]byte(nil), payload...))
	}
}

func (s *Session) receiveControl(p []byte, env *control.Envelope) {
	if env.Control == nil || env.Control.Flags != 0 || env.Control.Subtype == 0 {
		return
	}
	s.mu.Lock()
	if !s.ready || s.stopped || env.Local != s.peer || env.Peer != s.local {
		s.mu.Unlock()
		return
	}
	cb := s.controlCallback
	s.mu.Unlock()
	if cb == nil {
		return
	}
	payload := append([]byte(nil), p[control.EnvelopeSize:]...)
	go cb(env.Control.Subtype, payload)
}

// acceptSequenceLocked implements the 64-entry sliding replay window.
// Caller holds s.mu.
func (s *Session) acceptSequenceLocked(seq uint64) bool {
	if seq == 0 {
		return false
	}
	if seq > s.highest {
		gap := seq - s.highest
		if gap >= 64 {
			s.window = 0
		} else {
			s.window <<= gap
		}
		s.highest = seq
		s.window |= 1
		return true
	}
	gap := s.highest - seq
	if gap >= 64 || s.window&(uint64(1)<<gap) != 0 {
		return false
	}
	s.window |= uint64(1) << gap
	return true
}

// Stats aggregates counters from all transports.
func (s *Session) Stats() TransportStats {
	s.mu.Lock()
	links := make([]*transportLink, 0, len(s.links))
	for _, l := range s.links {
		links = append(links, l)
	}
	s.mu.Unlock()

	var out TransportStats
	for _, l := range links {
		st := l.raw.Stats()
		out.BytesSent += st.BytesSent
		out.BytesReceived += st.BytesReceived
		out.PacketsSent += st.PacketsSent
		out.PacketsRecv += st.PacketsRecv
		out.Reconnects += st.Reconnects
	}
	out.Connected = s.IsConnected()
	return out
}

// MarkDead marks a transport as unavailable for routing. Called externally
// when a transport's own error handling decides it cannot recover.
func (s *Session) MarkDead(name string) {
	s.mu.Lock()
	if l, ok := s.links[name]; ok {
		l.dead = true
	}
	s.mu.Unlock()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
