package socks5

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"

	"openflux/utils"
)

type Dialer interface {
	DialTCP(address string) (net.Conn, error)
	DialUDP(address string) (net.Conn, error)
}

type SOCKS5Server struct {
	listenAddr string
	dialer     Dialer

	mu       sync.Mutex
	listener net.Listener
	closed   bool
}

func NewSOCKS5Server(addr string, dialer Dialer) *SOCKS5Server {
	return &SOCKS5Server{listenAddr: addr, dialer: dialer}
}

// Bind reserves the listen address so callers can detect "address already in
// use" synchronously, before serving. Safe to call once; Start binds lazily if
// it wasn't called.
func (s *SOCKS5Server) Bind() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	if s.listener != nil {
		return nil
	}
	listener, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return err
	}
	s.listener = listener
	return nil
}

func (s *SOCKS5Server) Start() error {
	if err := s.Bind(); err != nil {
		return err
	}

	s.mu.Lock()
	listener := s.listener
	s.mu.Unlock()
	defer listener.Close()

	utils.Debugf("[SOCKS5] Listening on %s", s.listenAddr)

	for {
		conn, err := listener.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				utils.Debugf("[SOCKS5] Listener closed, stopping")
				return net.ErrClosed
			}
			utils.Debugf("[SOCKS5] Accept error: %v", err)
			continue
		}
		go s.handleConnection(conn)
	}
}

// Close stops the server, unblocking Start's accept loop.
func (s *SOCKS5Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

func (s *SOCKS5Server) handleConnection(clientConn net.Conn) {
	// A malformed request must never crash the host process; contain any
	// panic to this connection.
	defer func() {
		if r := recover(); r != nil {
			utils.Debugf("[SOCKS5] Recovered from panic in handler: %v", r)
		}
	}()
	defer clientConn.Close()

	var greeting [2]byte
	if _, err := io.ReadFull(clientConn, greeting[:]); err != nil || greeting[0] != 0x05 {
		return
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(clientConn, methods); err != nil {
		return
	}
	noAuth := false
	for _, method := range methods {
		if method == 0x00 {
			noAuth = true
			break
		}
	}
	if !noAuth {
		_, _ = clientConn.Write([]byte{0x05, 0xff})
		return
	}
	if _, err := clientConn.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	var request [4]byte
	if _, err := io.ReadFull(clientConn, request[:]); err != nil || request[0] != 0x05 {
		return
	}
	targetAddr, err := readAddress(clientConn, request[3])
	if err != nil {
		writeReply(clientConn, 0x08, nil)
		return
	}

	switch request[1] {
	case 0x01:
		s.handleConnect(clientConn, targetAddr)
	case 0x03:
		s.handleUDPAssociate(clientConn)
	default:
		writeReply(clientConn, 0x07, nil)
	}
}

func (s *SOCKS5Server) handleConnect(clientConn net.Conn, targetAddr string) {

	utils.Debugf("[SOCKS5] CONNECT %s", targetAddr)

	targetConn, err := s.dialer.DialTCP(targetAddr)
	if err != nil {
		utils.Debugf("[SOCKS5] Dial failed: %v", err)
		writeReply(clientConn, 0x04, nil)
		return
	}
	defer targetConn.Close()

	writeReply(clientConn, 0x00, targetConn.LocalAddr())

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		defer targetConn.Close()
		io.Copy(targetConn, clientConn)
	}()

	go func() {
		defer wg.Done()
		defer clientConn.Close()
		io.Copy(clientConn, targetConn)
	}()

	wg.Wait()
}

func (s *SOCKS5Server) handleUDPAssociate(control net.Conn) {
	bindIP := net.ParseIP("127.0.0.1")
	if host, _, err := net.SplitHostPort(control.LocalAddr().String()); err == nil {
		if parsed := net.ParseIP(host); parsed != nil {
			bindIP = parsed
		}
	}
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: bindIP})
	if err != nil {
		writeReply(control, 0x01, nil)
		return
	}
	defer udpConn.Close()
	if err := writeReply(control, 0x00, udpConn.LocalAddr()); err != nil {
		return
	}

	type udpFlow struct{ conn net.Conn }
	flows := make(map[string]udpFlow)
	var flowsMu sync.Mutex
	var clientAddr *net.UDPAddr
	var clientMu sync.RWMutex
	expectedIP := net.ParseIP("127.0.0.1")
	if host, _, err := net.SplitHostPort(control.RemoteAddr().String()); err == nil {
		expectedIP = net.ParseIP(host)
	}
	defer func() {
		flowsMu.Lock()
		defer flowsMu.Unlock()
		for _, flow := range flows {
			_ = flow.conn.Close()
		}
	}()

	utils.SafeGo("socks5.udp", func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := udpConn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if expectedIP != nil && !from.IP.Equal(expectedIP) {
				continue
			}
			clientMu.RLock()
			pinnedClient := clientAddr
			clientMu.RUnlock()
			if pinnedClient != nil &&
				(!from.IP.Equal(pinnedClient.IP) || from.Port != pinnedClient.Port) {
				continue
			}
			dest, payload, err := parseUDPRequest(buf[:n])
			if err != nil {
				continue
			}
			clientMu.Lock()
			clientAddr = from
			clientMu.Unlock()

			flowsMu.Lock()
			flow, ok := flows[dest]
			if !ok {
				if len(flows) >= 256 {
					flowsMu.Unlock()
					continue
				}
				conn, err := s.dialer.DialUDP(dest)
				if err != nil {
					flowsMu.Unlock()
					continue
				}
				flow = udpFlow{conn: conn}
				flows[dest] = flow
				utils.SafeGo("socks5.udp-response", func() {
					response := make([]byte, 65535)
					for {
						n, err := conn.Read(response)
						if err != nil {
							return
						}
						packet := makeUDPResponse(conn.RemoteAddr(), response[:n])
						clientMu.RLock()
						to := clientAddr
						clientMu.RUnlock()
						if to != nil {
							_, _ = udpConn.WriteToUDP(packet, to)
						}
					}
				})
			}
			flowsMu.Unlock()
			_, _ = flow.conn.Write(payload)
		}
	})

	_, _ = io.Copy(io.Discard, control)
}

func readAddress(r io.Reader, atyp byte) (string, error) {
	var host string
	switch atyp {
	case 0x01:
		buf := make([]byte, 4)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		host = net.IP(buf).String()
	case 0x03:
		var size [1]byte
		if _, err := io.ReadFull(r, size[:]); err != nil || size[0] == 0 {
			return "", fmt.Errorf("invalid domain length")
		}
		buf := make([]byte, int(size[0]))
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		host = string(buf)
	case 0x04:
		buf := make([]byte, 16)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		host = net.IP(buf).String()
	default:
		return "", fmt.Errorf("unsupported address type %d", atyp)
	}
	var port [2]byte
	if _, err := io.ReadFull(r, port[:]); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, fmt.Sprintf("%d", binary.BigEndian.Uint16(port[:]))), nil
}

func writeReply(w io.Writer, code byte, addr net.Addr) error {
	ip := net.IPv4zero
	port := 0
	if host, service, err := net.SplitHostPort(addressString(addr)); err == nil {
		if parsed := net.ParseIP(host).To4(); parsed != nil {
			ip = parsed
		}
		fmt.Sscanf(service, "%d", &port)
	}
	reply := []byte{0x05, code, 0x00, 0x01, ip[0], ip[1], ip[2], ip[3], byte(port >> 8), byte(port)}
	_, err := w.Write(reply)
	return err
}

func addressString(addr net.Addr) string {
	if addr == nil {
		return "0.0.0.0:0"
	}
	return addr.String()
}

func parseUDPRequest(packet []byte) (string, []byte, error) {
	if len(packet) < 4 || packet[0] != 0 || packet[1] != 0 || packet[2] != 0 {
		return "", nil, fmt.Errorf("invalid or fragmented SOCKS5 UDP packet")
	}
	r := &sliceReader{data: packet[4:]}
	addr, err := readAddress(r, packet[3])
	if err != nil {
		return "", nil, err
	}
	return addr, r.data, nil
}

type sliceReader struct{ data []byte }

func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func makeUDPResponse(addr net.Addr, payload []byte) []byte {
	ip := net.IPv4zero
	port := 0
	if host, service, err := net.SplitHostPort(addressString(addr)); err == nil {
		if parsed := net.ParseIP(host).To4(); parsed != nil {
			ip = parsed
		}
		fmt.Sscanf(service, "%d", &port)
	}
	out := []byte{0, 0, 0, 0x01, ip[0], ip[1], ip[2], ip[3], byte(port >> 8), byte(port)}
	return append(out, payload...)
}
