package tunnel

// OnOutgoing sets where the stack's outgoing packets go, for a stack that
// is not attached to a transport (the phone's own stack for direct
// connections, mobile/routing). Set it before the endpoint gets a NIC.
func (e *TunnelLinkEndpoint) OnOutgoing(f func([]byte)) { e.onOutgoingPacket = f }
