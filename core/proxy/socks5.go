package proxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/gohome/network"
	"github.com/esrrhs/yellowsocks/core/dns"
	"github.com/esrrhs/yellowsocks/core/router"
	"github.com/esrrhs/yellowsocks/core/stats"
)

// UpstreamProvider provides upstream proxy address (e.g. from SPP Manager)
type UpstreamProvider interface {
	Socks5Addr() string
}

// Socks5Config configures the SOCKS5 server
type Socks5Config struct {
	ListenAddr string
	Username   string
	Password   string
	Router     *router.Router
	DNS        *dns.Server
	Upstream   UpstreamProvider
}

// Socks5Server provides inbound SOCKS5 proxy supporting TCP and UDP with smart routing
type Socks5Server struct {
	cfg      Socks5Config
	listener net.Listener
	mu       sync.Mutex
	closed   bool
	stopCh   chan struct{}
}

// NewSocks5Server creates a new SOCKS5 server instance
func NewSocks5Server(cfg Socks5Config) *Socks5Server {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:1080"
	}
	return &Socks5Server{
		cfg:    cfg,
		stopCh: make(chan struct{}),
	}
}

// Start begins listening on the configured TCP address
func (s *Socks5Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.listener != nil {
		return nil
	}

	l, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("socks5 listen failed: %w", err)
	}
	s.listener = l
	s.closed = false

	loggo.Info("[SOCKS5] Server listening on %s (TCP CONNECT + UDP ASSOCIATE)", s.cfg.ListenAddr)

	go s.acceptLoop(l)
	return nil
}

// Addr returns the actual listening address
func (s *Socks5Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Addr()
	}
	return nil
}

// Stop closes the server and terminates active sessions
func (s *Socks5Server) Stop() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	l := s.listener
	s.listener = nil
	s.mu.Unlock()

	if l != nil {
		return l.Close()
	}
	return nil
}

func (s *Socks5Server) acceptLoop(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return
			default:
				if s.closed {
					return
				}
				time.Sleep(10 * time.Millisecond)
				continue
			}
		}

		go s.handleClient(conn)
	}
}

func (s *Socks5Server) handleClient(clientConn net.Conn) {
	defer common.CrashLog()
	defer clientConn.Close()

	_ = clientConn.SetDeadline(time.Now().Add(10 * time.Second))

	// 1. Handshake & authentication
	if err := network.Sock5HandshakeBy(clientConn, s.cfg.Username, s.cfg.Password); err != nil {
		loggo.Debug("[SOCKS5] Handshake error from %s: %v", clientConn.RemoteAddr(), err)
		return
	}

	// 2. Read SOCKS5 request header (VER, CMD, RSV, ATYP)
	var header [4]byte
	if _, err := io.ReadFull(clientConn, header[:]); err != nil {
		return
	}
	if header[0] != 0x05 {
		return
	}

	cmd := header[1]
	atyp := header[3]

	// 3. Read target address
	targetHost, targetPort, err := readSocks5Addr(clientConn, atyp)
	if err != nil {
		_ = network.Sock5SendConnectReply(clientConn, 0x08, "0.0.0.0:0")
		return
	}

	// Remove deadline for ongoing payload transfer
	_ = clientConn.SetDeadline(time.Time{})

	switch cmd {
	case network.Socks5CmdConnect:
		s.handleConnect(clientConn, targetHost, targetPort)
	case network.Socks5CmdUDPAssociate:
		s.handleUDPAssociate(clientConn, targetHost, targetPort)
	default:
		// Command not supported
		_ = network.Sock5SendConnectReply(clientConn, 0x07, "0.0.0.0:0")
	}
}

func readSocks5Addr(r io.Reader, atyp byte) (string, int, error) {
	var host string

	switch atyp {
	case 0x01: // IPv4
		var ip [4]byte
		if _, err := io.ReadFull(r, ip[:]); err != nil {
			return "", 0, err
		}
		host = net.IP(ip[:]).String()
	case 0x03: // Domain name
		var lenBuf [1]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			return "", 0, err
		}
		domainLen := int(lenBuf[0])
		domainBuf := make([]byte, domainLen)
		if _, err := io.ReadFull(r, domainBuf); err != nil {
			return "", 0, err
		}
		host = string(domainBuf)
	case 0x04: // IPv6
		var ip [16]byte
		if _, err := io.ReadFull(r, ip[:]); err != nil {
			return "", 0, err
		}
		host = net.IP(ip[:]).String()
	default:
		return "", 0, fmt.Errorf("unsupported atyp 0x%02x", atyp)
	}

	var portBuf [2]byte
	if _, err := io.ReadFull(r, portBuf[:]); err != nil {
		return "", 0, err
	}
	port := int(portBuf[0])<<8 | int(portBuf[1])

	return host, port, nil
}

func (s *Socks5Server) routeDecision(host string, port int) (router.RouteDecision, string) {
	realHost := host
	if s.cfg.DNS != nil {
		if d, ok := s.cfg.DNS.LookupDomainByIP(host); ok {
			realHost = d
		}
	}

	parsedIP := net.ParseIP(realHost)
	decision := router.Proxy
	if s.cfg.Router != nil {
		decision = s.cfg.Router.Decide(realHost, parsedIP)
	}
	return decision, realHost
}

func (s *Socks5Server) handleConnect(clientConn net.Conn, targetHost string, targetPort int) {
	decision, realHost := s.routeDecision(targetHost, targetPort)
	targetAddr := net.JoinHostPort(targetHost, strconv.Itoa(targetPort))
	ruleStr := "Proxy (SPP)"
	if decision == router.Direct {
		ruleStr = "Direct"
	}

	connID := clientConn.RemoteAddr().String() + "->" + targetAddr
	stats.Default.TrackConnection(connID, "socks5", clientConn.RemoteAddr().String(), targetAddr, realHost, ruleStr)
	defer stats.Default.RemoveConnection(connID)

	loggo.Info("[SOCKS5] TCP CONNECT %s -> %s (%s, Decision: %s)",
		clientConn.RemoteAddr(), targetAddr, realHost, ruleStr)

	if decision == router.Direct {
		outConn, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
		if err != nil {
			loggo.Warn("[SOCKS5] Direct dial %s failed: %v", targetAddr, err)
			_ = network.Sock5SendConnectReply(clientConn, 0x05, "0.0.0.0:0")
			return
		}
		defer outConn.Close()

		if err := network.Sock5SendConnectReply(clientConn, 0x00, "0.0.0.0:0"); err != nil {
			return
		}
		relayStreams(clientConn, outConn)
	} else {
		// Forward via SPP
		if s.cfg.Upstream == nil || s.cfg.Upstream.Socks5Addr() == "" {
			loggo.Error("[SOCKS5] No upstream SPP proxy available for proxy route to %s", targetAddr)
			_ = network.Sock5SendConnectReply(clientConn, 0x01, "0.0.0.0:0")
			return
		}

		sppTCPAddr, err := net.ResolveTCPAddr("tcp", s.cfg.Upstream.Socks5Addr())
		if err != nil {
			loggo.Error("[SOCKS5] Resolve SPP upstream %s failed: %v", s.cfg.Upstream.Socks5Addr(), err)
			_ = network.Sock5SendConnectReply(clientConn, 0x01, "0.0.0.0:0")
			return
		}

		sppConn, err := net.DialTCP("tcp", nil, sppTCPAddr)
		if err != nil {
			loggo.Error("[SOCKS5] Dial SPP upstream %s failed: %v", s.cfg.Upstream.Socks5Addr(), err)
			_ = network.Sock5SendConnectReply(clientConn, 0x01, "0.0.0.0:0")
			return
		}
		defer sppConn.Close()

		if err := network.Sock5Handshake(sppConn, 5000, "", ""); err != nil {
			loggo.Error("[SOCKS5] SPP upstream handshake failed: %v", err)
			_ = network.Sock5SendConnectReply(clientConn, 0x01, "0.0.0.0:0")
			return
		}

		if err := network.Sock5SetRequest(sppConn, targetHost, targetPort, 10000); err != nil {
			loggo.Error("[SOCKS5] SPP upstream connect to %s failed: %v", targetAddr, err)
			_ = network.Sock5SendConnectReply(clientConn, 0x01, "0.0.0.0:0")
			return
		}

		if err := network.Sock5SendConnectReply(clientConn, 0x00, "0.0.0.0:0"); err != nil {
			return
		}
		relayStreams(clientConn, sppConn)
	}
}

func (s *Socks5Server) handleUDPAssociate(clientConn net.Conn, targetHost string, targetPort int) {
	// 1. Create local UDP relay listener
	relayConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		_ = network.Sock5SendConnectReply(clientConn, 0x01, "0.0.0.0:0")
		return
	}
	defer relayConn.Close()

	// Determine relay address to reply to client
	relayPort := relayConn.LocalAddr().(*net.UDPAddr).Port
	relayHost := "127.0.0.1"
	if tcpLocal := clientConn.LocalAddr(); tcpLocal != nil {
		if h, _, err := net.SplitHostPort(tcpLocal.String()); err == nil {
			ip := net.ParseIP(h)
			if ip != nil && !ip.IsUnspecified() {
				relayHost = h
			}
		}
	}
	bndAddr := net.JoinHostPort(relayHost, strconv.Itoa(relayPort))

	if err := network.Sock5SendConnectReply(clientConn, 0x00, bndAddr); err != nil {
		return
	}

	loggo.Info("[SOCKS5] UDP ASSOCIATE ready on %s for client TCP %s", bndAddr, clientConn.RemoteAddr())

	assocClosed := atomic.Bool{}
	activeClientUDP := atomic.Pointer[net.UDPAddr]{}

	// Watch client TCP connection: RFC 1928 stipulates UDP association terminates when TCP connection dies
	go func() {
		defer common.CrashLog()
		dummy := make([]byte, 1)
		for {
			_, err := clientConn.Read(dummy)
			if err != nil {
				assocClosed.Store(true)
				relayConn.Close()
				return
			}
		}
	}()

	// Upstream SPP UDP session tracking
	var sppMu sync.Mutex
	var sppTCPConn net.Conn
	var sppUDPConn *net.UDPConn
	var sppRelayUDP *net.UDPAddr

	closeSPP := func() {
		sppMu.Lock()
		defer sppMu.Unlock()
		if sppTCPConn != nil {
			_ = sppTCPConn.Close()
			sppTCPConn = nil
		}
		if sppUDPConn != nil {
			_ = sppUDPConn.Close()
			sppUDPConn = nil
		}
	}
	defer closeSPP()

	getOrCreateSPPUDPSession := func() (*net.UDPConn, *net.UDPAddr, error) {
		sppMu.Lock()
		defer sppMu.Unlock()
		if sppUDPConn != nil && sppRelayUDP != nil {
			return sppUDPConn, sppRelayUDP, nil
		}
		if s.cfg.Upstream == nil || s.cfg.Upstream.Socks5Addr() == "" {
			return nil, nil, errors.New("no upstream SPP socks5 available")
		}

		tcpUpAddr, err := net.ResolveTCPAddr("tcp", s.cfg.Upstream.Socks5Addr())
		if err != nil {
			return nil, nil, err
		}
		tcpUp, err := net.DialTCP("tcp", nil, tcpUpAddr)
		if err != nil {
			return nil, nil, err
		}

		if err := network.Sock5Handshake(tcpUp, 5000, "", ""); err != nil {
			_ = tcpUp.Close()
			return nil, nil, err
		}

		bnd, err := network.Sock5SetUDPRequest(tcpUp, "0.0.0.0", 0, 5000)
		if err != nil {
			_ = tcpUp.Close()
			return nil, nil, err
		}

		bndH, bndP, err := net.SplitHostPort(bnd)
		if err != nil {
			_ = tcpUp.Close()
			return nil, nil, err
		}
		p, _ := strconv.Atoi(bndP)
		bndIP := net.ParseIP(bndH)
		if bndIP == nil || bndIP.IsUnspecified() {
			bndIP = tcpUp.RemoteAddr().(*net.TCPAddr).IP
		}
		relayTarget := &net.UDPAddr{IP: bndIP, Port: p}

		uConn, err := net.ListenUDP("udp", nil)
		if err != nil {
			_ = tcpUp.Close()
			return nil, nil, err
		}

		sppTCPConn = tcpUp
		sppUDPConn = uConn
		sppRelayUDP = relayTarget

		// Receive responses from SPP UDP relay and forward back to client
		go func() {
			defer common.CrashLog()
			buf := make([]byte, 65535)
			for {
				n, _, err := uConn.ReadFromUDP(buf)
				if err != nil || assocClosed.Load() {
					return
				}
				cAddr := activeClientUDP.Load()
				if cAddr != nil && n > 0 {
					_, _ = relayConn.WriteToUDP(buf[:n], cAddr)
				}
			}
		}()

		return sppUDPConn, sppRelayUDP, nil
	}

	// Direct UDP session tracking (target -> net.UDPConn)
	var directMu sync.Mutex
	directSessions := make(map[string]*net.UDPConn)
	defer func() {
		directMu.Lock()
		defer directMu.Unlock()
		for _, c := range directSessions {
			_ = c.Close()
		}
	}()

	getOrCreateDirectConn := func(targetAddr string) (*net.UDPConn, error) {
		directMu.Lock()
		defer directMu.Unlock()
		if c, ok := directSessions[targetAddr]; ok {
			return c, nil
		}
		dConn, err := net.ListenUDP("udp", nil)
		if err != nil {
			return nil, err
		}
		directSessions[targetAddr] = dConn

		// Receive responses from direct target and pack back to client
		go func() {
			defer common.CrashLog()
			buf := make([]byte, 65535)
			for {
				n, rAddr, err := dConn.ReadFromUDP(buf)
				if err != nil || assocClosed.Load() {
					return
				}
				cAddr := activeClientUDP.Load()
				if cAddr != nil && n > 0 {
					packed, err := network.Sock5PackUDP(rAddr.IP.String(), rAddr.Port, buf[:n])
					if err == nil {
						_, _ = relayConn.WriteToUDP(packed, cAddr)
					}
				}
			}
		}()
		return dConn, nil
	}

	// Read UDP packets sent from client to relay socket
	pktBuf := make([]byte, 65535)
	for {
		n, clientSrcUDP, err := relayConn.ReadFromUDP(pktBuf)
		if err != nil {
			if assocClosed.Load() {
				return
			}
			continue
		}
		if n <= 0 {
			continue
		}

		activeClientUDP.Store(clientSrcUDP)

		dstHost, dstPort, payload, err := network.Sock5UnpackUDP(pktBuf[:n])
		if err != nil || len(payload) == 0 {
			continue
		}

		decision, realHost := s.routeDecision(dstHost, dstPort)
		targetAddr := net.JoinHostPort(dstHost, strconv.Itoa(dstPort))

		if decision == router.Direct {
			dConn, err := getOrCreateDirectConn(targetAddr)
			if err != nil {
				continue
			}
			udpDst, err := net.ResolveUDPAddr("udp", targetAddr)
			if err != nil {
				continue
			}
			_, _ = dConn.WriteToUDP(payload, udpDst)
		} else {
			// Proxy via SPP
			sppUConn, sppRelayTarget, err := getOrCreateSPPUDPSession()
			if err != nil {
				loggo.Warn("[SOCKS5] SPP UDP session error for %s (%s): %v", targetAddr, realHost, err)
				continue
			}
			_, _ = sppUConn.WriteToUDP(pktBuf[:n], sppRelayTarget)
		}
	}
}

func relayStreams(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if tcp, ok := dst.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}

	go pipe(a, b)
	go pipe(b, a)
	wg.Wait()
}
