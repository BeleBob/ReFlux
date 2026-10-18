package mobile

import (
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/p1neappleXpress/OpenFlux/routing"
	"github.com/p1neappleXpress/OpenFlux/tunnel"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// localStack ends the device's direct (and blocked) connections on the
// phone and opens the direct ones from the app, whose sockets the VPN
// leaves out. It is the exit node's l4 backend in miniature.
type localStack struct {
	pr       *packetRouter
	s        *stack.Stack
	ep       *tunnel.TunnelLinkEndpoint
	udpFlows atomic.Int32
	closed   atomic.Bool
}

const (
	// directDialTimeout: a plain direct site; checkedDialTimeout: a
	// whitelisted one, which goes to the tunnel when it does not connect
	// before the device sends its SYN again (about a second, then 2 s more).
	directDialTimeout  = 10 * time.Second
	checkedDialTimeout = 2500 * time.Millisecond
	// checkedAnswerTimeoutDefault: a whitelisted site that took the connection
	// but has not answered the device's first bytes by then is blocked
	// after all (by name, the way whitelists also work).
	checkedAnswerTimeoutDefault = 6 * time.Second
	maxLocalUDPFlows            = 256
	localUDPIdle                = 2 * time.Minute
)

// checkedAnswerTimeout is checkedAnswerTimeoutDefault; tests shorten it.
var checkedAnswerTimeout = checkedAnswerTimeoutDefault

// dialDirect opens a connection from the app; tests replace it.
var dialDirect = func(network, addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout(network, addr, timeout)
}

func newLocalStack(pr *packetRouter) *localStack {
	l := &localStack{pr: pr}
	l.s = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	// The device is a hop away, not across a document relay: gVisor's
	// defaults with SACK and windows fit for a fast direct download.
	sack := tcpip.TCPSACKEnabled(true)
	l.s.SetTransportProtocolOption(tcp.ProtocolNumber, &sack)
	rcv := tcpip.TCPReceiveBufferSizeRangeOption{Min: 4 << 10, Default: 1 << 20, Max: 8 << 20}
	l.s.SetTransportProtocolOption(tcp.ProtocolNumber, &rcv)
	snd := tcpip.TCPSendBufferSizeRangeOption{Min: 4 << 10, Default: 1 << 20, Max: 8 << 20}
	l.s.SetTransportProtocolOption(tcp.ProtocolNumber, &snd)
	moderate := tcpip.TCPModerateReceiveBufferOption(true)
	l.s.SetTransportProtocolOption(tcp.ProtocolNumber, &moderate)

	l.ep = tunnel.NewTunnelLinkEndpoint()
	l.ep.OnOutgoing(func(b []byte) {
		if !l.closed.Load() {
			l.pr.fromLocal(append([]byte(nil), b...))
		}
	})
	nic := tcpip.NICID(1)
	if err := l.s.CreateNIC(nic, l.ep); err != nil {
		utils.Infof("[ROUTING] local stack: %v", err)
		return l
	}
	l.s.SetPromiscuousMode(nic, true)
	l.s.SetSpoofing(nic, true)
	l.s.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: nic})
	fwd := tcp.NewForwarder(l.s, 0, 1024, l.handleTCP)
	l.s.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
	ufwd := udp.NewForwarder(l.s, l.handleUDP)
	l.s.SetTransportProtocolHandler(udp.ProtocolNumber, ufwd.HandlePacket)
	return l
}

func (l *localStack) inject(b []byte, _ routing.Action) {
	if !l.closed.Load() {
		l.ep.InjectInbound(b)
	}
}

func (l *localStack) close() {
	if l.closed.Swap(true) {
		return
	}
	l.s.Close()
}

func addrOf(a tcpip.Address) netip.Addr {
	return netip.AddrFrom4(a.As4())
}

func (l *localStack) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	client, server := addrOf(id.RemoteAddress), addrOf(id.LocalAddress)
	action, host, ok := l.pr.flowOf(client, server, id.RemotePort, id.LocalPort, protoTCP)
	dest := net.JoinHostPort(server.String(), strconv.Itoa(int(id.LocalPort)))
	if !ok || action == routing.Block || action == routing.Proxy {
		r.Complete(true)
		return
	}
	timeout := directDialTimeout
	if action == routing.DirectChecked {
		timeout = checkedDialTimeout
	}
	remote, err := dialDirect("tcp", dest, timeout)
	if err != nil {
		if action == routing.DirectChecked {
			// Through the tunnel from the next SYN on; the reset this
			// sends never reaches the device (fromLocal).
			l.pr.router.MarkFailed(host)
			l.pr.moveToTunnel(client, server, id.RemotePort, id.LocalPort, protoTCP)
			utils.Debugf("[ROUTING] %s (%s) does not connect direct, now through the tunnel: %v", host, dest, err)
		} else {
			utils.Debugf("[ROUTING] direct %s (%s): %v", host, dest, err)
		}
		r.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, tErr := r.CreateEndpoint(&wq)
	if tErr != nil {
		remote.Close()
		r.Complete(true)
		return
	}
	r.Complete(false)
	local := gonet.NewTCPConn(&wq, ep)
	if tc, ok := remote.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	var answered, watching atomic.Bool
	var once sync.Once
	closeBoth := func() { once.Do(func() { local.Close(); remote.Close() }) }
	// A whitelisted site that took the connection has checkedAnswerTimeout
	// from the device's first bytes to answer.
	watch := time.AfterFunc(time.Hour, func() {
		if !answered.Load() {
			l.pr.router.MarkFailed(host)
			utils.Debugf("[ROUTING] %s (%s) took the connection but does not answer: through the tunnel from now on", host, dest)
			closeBoth()
		}
	})
	watch.Stop()
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := local.Read(buf)
			if n > 0 {
				if action == routing.DirectChecked && !answered.Load() && watching.CompareAndSwap(false, true) {
					watch.Reset(checkedAnswerTimeout)
				}
				if _, werr := remote.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		if tc, ok := remote.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			closeBoth()
		}
	}()
	buf := make([]byte, 64<<10)
	for {
		n, err := remote.Read(buf)
		if n > 0 {
			answered.Store(true)
			if _, werr := local.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	watch.Stop()
	closeBoth()
}

func (l *localStack) handleUDP(r *udp.ForwarderRequest) bool {
	id := r.ID()
	client, server := addrOf(id.RemoteAddress), addrOf(id.LocalAddress)
	action, _, ok := l.pr.flowOf(client, server, id.RemotePort, id.LocalPort, protoUDP)
	if !ok || action != routing.Direct {
		return true // blocked: dropped
	}
	if l.udpFlows.Add(1) > maxLocalUDPFlows {
		l.udpFlows.Add(-1)
		return true
	}
	dest := net.JoinHostPort(server.String(), strconv.Itoa(int(id.LocalPort)))
	var wq waiter.Queue
	ep, tErr := r.CreateEndpoint(&wq)
	if tErr != nil {
		l.udpFlows.Add(-1)
		return true
	}
	local := gonet.NewUDPConn(&wq, ep)
	go func() {
		defer l.udpFlows.Add(-1)
		remote, err := dialDirect("udp", dest, 5*time.Second)
		if err != nil {
			local.Close()
			return
		}
		refresh := func() {
			d := time.Now().Add(localUDPIdle)
			local.SetReadDeadline(d)
			remote.SetReadDeadline(d)
		}
		refresh()
		var once sync.Once
		closeBoth := func() { once.Do(func() { local.Close(); remote.Close() }) }
		pump := func(dst, src net.Conn) {
			defer closeBoth()
			buf := make([]byte, 64<<10)
			for {
				n, err := src.Read(buf)
				if err != nil {
					return
				}
				refresh()
				if _, err := dst.Write(buf[:n]); err != nil {
					return
				}
			}
		}
		go pump(remote, local)
		pump(local, remote)
	}()
	return true
}
