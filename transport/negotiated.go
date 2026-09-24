package transport

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Negotiation is inside EncryptedTransport: neither control fields nor data
// session IDs are accepted before AEAD authentication. Legacy wire-v3 controls
// are deliberately not used. This does not replace the existing key schedule.
const (
	CapabilityICMPErrors Capabilities = 1 << 4
	negotiationHeader                 = 78
	MaxNegotiatedPacket               = 65000 // Leaves space for this envelope and AES in a 65535-byte record.
)

var negotiationMagic = []byte{'O', 'F', 'N', 1}
var ErrNegotiationPending = errors.New("authenticated peer negotiation is not complete")

type PeerParameters struct {
	Capabilities  Capabilities
	MaxPacketSize int
}

type PeerParameterProvider interface{ PeerParameters() (PeerParameters, bool) }

type NegotiatedTransport struct {
	*EncryptedTransport
	mu               sync.Mutex
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
	callback         func([]byte)
	done             chan struct{}
	once             sync.Once
	wg               sync.WaitGroup
	handshakeTimeout time.Duration
}

func NewNegotiatedTransport(inner *EncryptedTransport, p PeerParameters, exit bool) (*NegotiatedTransport, error) {
	if inner == nil || !validParameters(p) {
		return nil, errors.New("negotiation requires encryption, IPv4/TCP and a packet limit from 1280 to 65000")
	}
	if b, ok := inner.Transport.(*BatchedTransport); ok && b.experimentalV3 {
		return nil, errors.New("disable OPENFLUX_EXPERIMENTAL_WIRE_V3 when using authenticated negotiation")
	}
	n := &NegotiatedTransport{EncryptedTransport: inner, params: p, exit: exit, done: make(chan struct{}), handshakeTimeout: 20 * time.Second}
	if _, err := rand.Read(n.local[:]); err != nil {
		return nil, err
	}
	inner.Receive(n.receive)
	return n, nil
}

func validParameters(p PeerParameters) bool {
	const allowed = CapabilityIPv4 | CapabilityTCP | CapabilityUDP | CapabilityICMPErrors
	return p.Capabilities & ^allowed == 0 && p.Capabilities&(CapabilityIPv4|CapabilityTCP) == CapabilityIPv4|CapabilityTCP && p.MaxPacketSize >= 1280 && p.MaxPacketSize <= MaxNegotiatedPacket
}

func (n *NegotiatedTransport) Receive(cb func([]byte)) { n.mu.Lock(); n.callback = cb; n.mu.Unlock() }

func (n *NegotiatedTransport) Start() (err error) {
	n.mu.Lock()
	if n.stopped || n.started {
		n.mu.Unlock()
		return errors.New("negotiated transport already started or stopped")
	}
	n.started = true
	n.wg.Add(1)
	n.mu.Unlock()
	defer func() {
		n.wg.Done()
		if err != nil {
			_ = n.Stop()
		}
	}()
	if err := n.EncryptedTransport.Start(); err != nil {
		return err
	}
	timer := time.NewTimer(n.handshakeTimeout)
	defer timer.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := n.hello(); err != nil && n.EncryptedTransport.IsConnected() {
			return err
		}
		if n.IsConnected() {
			return nil
		}
		select {
		case <-n.done:
			return errors.New("negotiated transport stopped")
		case <-timer.C:
			return errors.New("peer negotiation timed out: check encryption key, codec and --negotiate on both peers; no legacy fallback")
		case <-tick.C:
		}
	}
}

func (n *NegotiatedTransport) Stop() error {
	n.once.Do(func() { n.mu.Lock(); n.stopped = true; n.ready = false; close(n.done); n.mu.Unlock() })
	err := n.EncryptedTransport.Stop()
	n.wg.Wait()
	return err
}

func (n *NegotiatedTransport) IsConnected() bool {
	n.mu.Lock()
	ready := n.ready && !n.stopped
	n.mu.Unlock()
	return ready && n.EncryptedTransport.IsConnected()
}

func (n *NegotiatedTransport) PeerParameters() (PeerParameters, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.remote, n.ready && !n.stopped
}

func (n *NegotiatedTransport) frame(kind byte) []byte {
	p := make([]byte, negotiationHeader)
	copy(p, negotiationMagic)
	p[4] = kind
	if n.exit {
		p[5] = 1
	}
	copy(p[6:38], n.local[:])
	copy(p[38:70], n.peer[:])
	return p
}

func (n *NegotiatedTransport) hello() error {
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return errors.New("negotiated transport stopped")
	}
	p := n.frame(1)
	binary.BigEndian.PutUint32(p[70:74], uint32(n.params.Capabilities))
	binary.BigEndian.PutUint16(p[74:76], uint16(n.params.MaxPacketSize))
	if n.ready {
		p[76] = 1
	}
	n.mu.Unlock()
	return n.EncryptedTransport.Send(p)
}

func (n *NegotiatedTransport) Send(p []byte) error {
	n.mu.Lock()
	if !n.ready || n.stopped {
		n.mu.Unlock()
		return ErrNegotiationPending
	}
	if err := permittedPacket(p, n.remote); err != nil {
		n.mu.Unlock()
		return err
	}
	if n.sequence == ^uint64(0) {
		n.mu.Unlock()
		return errors.New("session sequence exhausted; restart both peers")
	}
	n.sequence++
	out := n.frame(2)
	binary.BigEndian.PutUint64(out[70:78], n.sequence)
	out = append(out, p...)
	n.mu.Unlock()
	return n.EncryptedTransport.Send(out)
}

func permittedPacket(p []byte, limits PeerParameters) error {
	if len(p) < 20 || p[0]>>4 != 4 || int(p[0]&15)*4 < 20 || int(p[0]&15)*4 > len(p) || int(binary.BigEndian.Uint16(p[2:4])) != len(p) {
		return errors.New("negotiated mode requires complete IPv4 packets")
	}
	if len(p) > limits.MaxPacketSize {
		return fmt.Errorf("IPv4 packet exceeds negotiated maximum %d", limits.MaxPacketSize)
	}
	switch p[9] {
	case 6:
	case 17:
		if limits.Capabilities&CapabilityUDP == 0 {
			return errors.New("peer does not support UDP")
		}
	case 1:
		if limits.Capabilities&CapabilityICMPErrors == 0 {
			return errors.New("peer does not support ICMP errors")
		}
	default:
		return errors.New("unsupported IP protocol")
	}
	return nil
}

func (n *NegotiatedTransport) receive(p []byte) {
	if len(p) < negotiationHeader || !bytes.Equal(p[:4], negotiationMagic) || p[5] > 1 || (p[5] == 1) == n.exit {
		return
	}
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return
	}
	var sender [32]byte
	copy(sender[:], p[6:38])
	if sender == ([32]byte{}) {
		n.mu.Unlock()
		return
	}
	if p[4] == 1 {
		params := PeerParameters{Capabilities(binary.BigEndian.Uint32(p[70:74])), int(binary.BigEndian.Uint16(p[74:76]))}
		if len(p) != negotiationHeader || p[76] > 1 || p[77] != 0 || !validParameters(params) {
			n.mu.Unlock()
			return
		}
		// Once established, peer identity and policy are immutable until both
		// processes restart. An old authenticated hello cannot roll them back.
		if n.ready && (sender != n.peer || params.Capabilities&n.params.Capabilities != n.remote.Capabilities || min(params.MaxPacketSize, n.params.MaxPacketSize) != n.remote.MaxPacketSize) {
			n.mu.Unlock()
			return
		}
		echo := bytes.Equal(p[38:70], n.local[:])
		if !echo && !bytes.Equal(p[38:70], make([]byte, 32)) {
			n.mu.Unlock()
			return
		}
		changed := sender != n.peer
		wasReady := n.ready
		n.peer = sender
		if echo {
			n.remote = PeerParameters{params.Capabilities & n.params.Capabilities, min(params.MaxPacketSize, n.params.MaxPacketSize)}
			n.ready = true
		}
		n.mu.Unlock()
		// No reply loop: reply only to a new challenge or first confirmation.
		if changed || (!wasReady && echo) || (wasReady && p[76] == 0) {
			_ = n.hello()
		}
		return
	}
	if p[4] != 2 || !n.ready || sender != n.peer || !bytes.Equal(p[38:70], n.local[:]) || permittedPacket(p[negotiationHeader:], n.remote) != nil {
		n.mu.Unlock()
		return
	}
	seq := binary.BigEndian.Uint64(p[70:78])
	if seq == 0 {
		n.mu.Unlock()
		return
	}
	if seq > n.highest {
		gap := seq - n.highest
		if gap >= 64 {
			n.window = 0
		} else {
			n.window <<= gap
		}
		n.highest = seq
		n.window |= 1
	} else {
		gap := n.highest - seq
		if gap >= 64 || n.window&(uint64(1)<<gap) != 0 {
			n.mu.Unlock()
			return
		}
		n.window |= uint64(1) << gap
	}
	cb := n.callback
	n.mu.Unlock()
	if cb != nil {
		cb(p[negotiationHeader:])
	}
}

func (n *NegotiatedTransport) Stats() TransportStats {
	s := n.EncryptedTransport.Stats()
	s.Connected = n.IsConnected()
	return s
}
