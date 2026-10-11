package transport

// PeerID names the peer's current session (the challenge of its hello):
// it changes when a client connects anew, so an exit knows to tell it what
// a new client needs to hear. Zero while no handshake is complete.
func (s *Session) PeerID() [32]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || s.stopped {
		return [32]byte{}
	}
	return s.peer
}
