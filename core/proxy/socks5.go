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
	Socks5Auth() (string, string)
}

// SmartUpstreamProvider can dynamically select an upstream node per connection.
type SmartUpstreamProvider interface {
	UpstreamProvider
	PickNode() (addr string, name string, release func())
}

// NodeFailureReporter reports errors on a specific upstream node.
type NodeFailureReporter interface {
	ReportNodeFailure(nodeName string, err error)
}

// Socks5Config configures the SOCKS5 server
type Socks5Config struct {
	ListenAddr string
	Username   string
	Password   string
	Router     *router.Router
	DNS        *dns.Server
	Upstream   UpstreamProvider
	// Stats records traffic per session. Nil disables accounting.
	Stats *stats.Collector
	// ActiveNode returns the name of the SPP node currently carrying proxy
	// traffic, so forwarded bytes can be attributed to it. Optional.
	ActiveNode func() string
	// EnableIPv6 allows IPv6 destinations via SPP. When false they are rejected.
	EnableIPv6 bool
	// IdleTimeout closes a proxied connection that transfers no data for this
	// long. Zero defaults to defaultIdleTimeout.
	IdleTimeout time.Duration
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
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultIdleTimeout
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
			// Stop closes both stopCh and the listener; stopCh is closed first,
			// so a shutdown always lands in the returning branch without racing
			// on the s.closed flag.
			select {
			case <-s.stopCh:
				return
			default:
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

	return canonicalHost(host), port, nil
}

func (s *Socks5Server) routeDecision(host string, port int) (router.RouteDecision, string) {
	realHost := host
	// Parse the literal address before any DNS reverse-lookup: when host is an IP
	// learned from one of our own answers, CIDR/GeoIP rules must still apply.
	// Reparsing the learned domain would yield nil and silently bypass reserved-range
	// and country routing (e.g. a domain resolving to 127.0.0.1 would then be
	// sent through SPP instead of going direct).
	parsedIP := net.ParseIP(host)
	if s.cfg.DNS != nil {
		if d, ok := s.cfg.DNS.LookupDomainByIP(host); ok {
			realHost = d
		}
	}

	decision := router.Proxy
	if s.cfg.Router != nil {
		decision = s.cfg.Router.Decide(realHost, parsedIP)
	}
	return decision, realHost
}

// beginSession registers a relay with the statistics collector. The returned
// session is nil-safe: every method no-ops when statistics are disabled.
func (s *Socks5Server) beginSession(p stats.Proto, client, host string, port int, decision router.RouteDecision) *stats.Session {
	sd := stats.DecisionDirect
	if decision == router.Proxy {
		sd = stats.DecisionProxy
	}
	node := ""
	if sd == stats.DecisionProxy && s.cfg.ActiveNode != nil {
		node = s.cfg.ActiveNode()
	}
	return s.cfg.Stats.Begin(p, client, host, port, sd, node)
}

func (s *Socks5Server) handleConnect(clientConn net.Conn, targetHost string, targetPort int) {
	targetHost = canonicalHost(targetHost)
	decision, realHost := s.routeDecision(targetHost, targetPort)
	targetAddr := joinDialAddr(targetHost, targetPort)
	ruleStr := "Proxy (SPP)"
	if decision == router.Direct {
		ruleStr = "Direct"
	}

	loggo.Info("[SOCKS5] TCP CONNECT %s -> %s (%s, Decision: %s)",
		clientConn.RemoteAddr(), targetAddr, realHost, ruleStr)

	sess := s.beginSession(stats.ProtoSOCKS5, addrString(clientConn.RemoteAddr()), realHost, targetPort, decision)
	defer sess.End(nil)

	if decision == router.Direct {
		outConn, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
		if err != nil {
			loggo.Warn("[SOCKS5] Direct dial %s failed: %v", targetAddr, err)
			sess.Fail(fmt.Errorf("direct dial %s: %w", targetAddr, err))
			_ = network.Sock5SendConnectReply(clientConn, 0x05, "0.0.0.0:0")
			return
		}
		defer outConn.Close()

		if err := network.Sock5SendConnectReply(clientConn, 0x00, "0.0.0.0:0"); err != nil {
			return
		}
		relayStreams(clientConn, outConn, s.cfg.IdleTimeout, sess)
	} else {
		// Upstream SPP server cannot route IPv6 (no IPv6 connectivity).
		// When clients send IPv6 literals, reject with Network Unreachable (0x03)
		// so clients/Happy Eyeballs immediately failover to IPv4.
		if !s.cfg.EnableIPv6 && isIPv6Literal(targetHost) {
			loggo.Warn("[SOCKS5] Target is IPv6 literal %s, but IPv6 disabled (ipv6: false); replying Network Unreachable (0x03)", targetAddr)
			_ = network.Sock5SendConnectReply(clientConn, 0x03, "0.0.0.0:0")
			return
		}

		// Forward via SPP
		var sppAddr string
		var nodeName string
		var release func()
		if smart, ok := s.cfg.Upstream.(SmartUpstreamProvider); ok {
			sppAddr, nodeName, release = smart.PickNode()
		} else if s.cfg.Upstream != nil {
			sppAddr = s.cfg.Upstream.Socks5Addr()
		}
		if release != nil {
			defer release()
		}
		if nodeName != "" {
			sess.SetNode(nodeName)
		}

		if sppAddr == "" {
			loggo.Error("[SOCKS5] No upstream SPP proxy available for proxy route to %s", targetAddr)
			sess.Fail(errors.New("no upstream SPP proxy available"))
			_ = network.Sock5SendConnectReply(clientConn, 0x01, "0.0.0.0:0")
			return
		}

		sppTCPAddr, err := net.ResolveTCPAddr("tcp", sppAddr)
		if err != nil {
			loggo.Error("[SOCKS5] Resolve SPP upstream %s failed: %v", sppAddr, err)
			sess.Fail(fmt.Errorf("resolve SPP upstream: %w", err))
			_ = network.Sock5SendConnectReply(clientConn, 0x01, "0.0.0.0:0")
			return
		}

		sppConn, err := net.DialTCP("tcp", nil, sppTCPAddr)
		if err != nil {
			loggo.Error("[SOCKS5] Dial SPP upstream %s (%s) failed: %v", sppAddr, nodeName, err)
			if rep, ok := s.cfg.Upstream.(NodeFailureReporter); ok && nodeName != "" {
				rep.ReportNodeFailure(nodeName, err)
			} else if rep, ok := s.cfg.Upstream.(interface{ ReportFailure(error) }); ok {
				rep.ReportFailure(err)
			}
			sess.Fail(fmt.Errorf("dial SPP upstream: %w", err))
			_ = network.Sock5SendConnectReply(clientConn, 0x01, "0.0.0.0:0")
			return
		}
		defer sppConn.Close()

		user, pass := "", ""
		if s.cfg.Upstream != nil {
			user, pass = s.cfg.Upstream.Socks5Auth()
		}
		if err := network.Sock5Handshake(sppConn, 5000, user, pass); err != nil {
			loggo.Error("[SOCKS5] SPP upstream %s (%s) handshake failed: %v", sppAddr, nodeName, err)
			if rep, ok := s.cfg.Upstream.(NodeFailureReporter); ok && nodeName != "" {
				rep.ReportNodeFailure(nodeName, err)
			} else if rep, ok := s.cfg.Upstream.(interface{ ReportFailure(error) }); ok {
				rep.ReportFailure(err)
			}
			sess.Fail(fmt.Errorf("SPP upstream handshake: %w", err))
			_ = network.Sock5SendConnectReply(clientConn, 0x01, "0.0.0.0:0")
			return
		}

		if err := network.Sock5SetRequest(sppConn, canonicalHost(targetHost), targetPort, 10000); err != nil {
			loggo.Error("[SOCKS5] SPP upstream connect to %s via %s failed: %v", targetAddr, nodeName, err)
			sess.Fail(fmt.Errorf("SPP upstream connect: %w", err))
			_ = network.Sock5SendConnectReply(clientConn, 0x01, "0.0.0.0:0")
			return
		}

		if err := network.Sock5SendConnectReply(clientConn, 0x00, "0.0.0.0:0"); err != nil {
			return
		}
		relayStreams(clientConn, sppConn, s.cfg.IdleTimeout, sess)
	}
}

func (s *Socks5Server) handleUDPAssociate(clientConn net.Conn, targetHost string, targetPort int) {
	// Bind the relay on the client connection's address family. gohome's
	// SOCKS reply encoder writes ATYP IPv6 when bnd is an IPv6 address.
	relayConn, bndAddr, err := listenClientUDPRelay(clientConn)
	if err != nil {
		_ = network.Sock5SendConnectReply(clientConn, 0x01, "0.0.0.0:0")
		return
	}
	defer relayConn.Close()

	if err := network.Sock5SendConnectReply(clientConn, 0x00, bndAddr); err != nil {
		return
	}

	loggo.Info("[SOCKS5] UDP ASSOCIATE ready on %s for client TCP %s", bndAddr, clientConn.RemoteAddr())

	// One association carries datagrams for many destinations, so it is one
	// statistics session; individual datagrams are attributed to their own
	// target domain.
	udpSess := s.cfg.Stats.Begin(stats.ProtoUDP, addrString(clientConn.RemoteAddr()), "", 0, stats.DecisionNone, "")
	defer udpSess.End(nil)

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
	var sppRelease func()

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
		if sppRelease != nil {
			sppRelease()
			sppRelease = nil
		}
	}
	defer closeSPP()

	getOrCreateSPPUDPSession := func() (*net.UDPConn, *net.UDPAddr, error) {
		sppMu.Lock()
		defer sppMu.Unlock()
		if sppUDPConn != nil && sppRelayUDP != nil {
			return sppUDPConn, sppRelayUDP, nil
		}
		var sppAddr string
		var nodeName string
		var release func()
		if smart, ok := s.cfg.Upstream.(SmartUpstreamProvider); ok {
			sppAddr, nodeName, release = smart.PickNode()
		} else if s.cfg.Upstream != nil {
			sppAddr = s.cfg.Upstream.Socks5Addr()
		}
		if sppAddr == "" {
			if release != nil {
				release()
			}
			return nil, nil, errors.New("no upstream SPP socks5 available")
		}

		tcpUpAddr, err := net.ResolveTCPAddr("tcp", sppAddr)
		if err != nil {
			if release != nil {
				release()
			}
			return nil, nil, err
		}
		tcpUp, err := net.DialTCP("tcp", nil, tcpUpAddr)
		if err != nil {
			if release != nil {
				release()
			}
			return nil, nil, err
		}

		user, pass := "", ""
		if s.cfg.Upstream != nil {
			user, pass = s.cfg.Upstream.Socks5Auth()
		}
		if err := network.Sock5Handshake(tcpUp, 5000, user, pass); err != nil {
			_ = tcpUp.Close()
			if release != nil {
				release()
			}
			return nil, nil, err
		}

		bnd, err := network.Sock5SetUDPRequest(tcpUp, "0.0.0.0", 0, 5000)
		if err != nil {
			_ = tcpUp.Close()
			if release != nil {
				release()
			}
			return nil, nil, err
		}

		bndH, bndP, err := net.SplitHostPort(bnd)
		if err != nil {
			_ = tcpUp.Close()
			if release != nil {
				release()
			}
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
			if release != nil {
				release()
			}
			return nil, nil, err
		}

		sppTCPConn = tcpUp
		sppUDPConn = uConn
		sppRelayUDP = relayTarget
		sppRelease = release
		if nodeName != "" {
			udpSess.SetNode(nodeName)
		}

		// Receive responses from SPP UDP relay and forward back to client
		go func() {
			defer common.CrashLog()
			buf := make([]byte, 65535)
			for {
				n, rAddr, err := uConn.ReadFromUDP(buf)
				if err != nil || assocClosed.Load() {
					return
				}
				// Only the advertised relay endpoint may speak for the upstream;
				// uConn is an unconnected socket, so any host reaching the ephemeral
				// port could otherwise inject datagrams into the client's association.
				if !sameUDPAddr(rAddr, relayTarget) {
					continue
				}
				cAddr := activeClientUDP.Load()
				if cAddr != nil && n > 0 {
					_, _ = relayConn.WriteToUDP(buf[:n], cAddr)
					udpSess.AddDatagram("", stats.DecisionProxy, 0, int64(n))
				}
			}
		}()

		return sppUDPConn, sppRelayUDP, nil
	}

	// Direct UDP sessions, one socket (plus a read goroutine) per target. Bounded
	// by directUDPSessions: without a cap one association could pin an FD and
	// goroutine for every arbitrary target the client names.
	directSessions := newDirectUDPRegistry()
	defer directSessions.closeAll()

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

		// RFC 1928: a UDP relay must only accept datagrams from the host/port
		// that registered the association. The first datagram pins the client
		// endpoint; packets from any other source are dropped. This matters
		// when the SOCKS5 listener is bound to a non-loopback address.
		known := activeClientUDP.Load()
		if known == nil {
			activeClientUDP.Store(clientSrcUDP)
		} else if !sameUDPAddr(known, clientSrcUDP) {
			continue
		}

		dstHost, dstPort, payload, err := network.Sock5UnpackUDP(pktBuf[:n])
		if err != nil || len(payload) == 0 {
			continue
		}
		dstHost = canonicalHost(dstHost)

		decision, realHost := s.routeDecision(dstHost, dstPort)
		targetAddr := joinDialAddr(dstHost, dstPort)

		if decision == router.Direct {
			// The registry resolves the target once per association and hands
			// back the same endpoint for every send and for the reply filter.
			// Re-resolving a round-robin name (RFC 1794) on later datagrams
			// could send them to a second peer whose replies the read loop
			// would then drop.
			sess, fresh, err := directSessions.get(targetAddr)
			if err != nil {
				loggo.Warn("[SOCKS5] direct UDP session for %s rejected: %v", targetAddr, err)
				continue
			}
			if fresh {
				go func(wantAddr *net.UDPAddr, conn *net.UDPConn, dec stats.Decision) {
					defer common.CrashLog()
					// Only datagrams from the target we sent to may be relayed; the
					// outbound socket is unconnected, so anything else reaching its
					// ephemeral port (a local process or off-path sender) would be
					// forwarded with a spoofed source address.
					buf := make([]byte, 65535)
					for {
						n, rAddr, err := conn.ReadFromUDP(buf)
						if err != nil || assocClosed.Load() {
							return
						}
						if !sameUDPAddr(rAddr, wantAddr) {
							continue
						}
						cAddr := activeClientUDP.Load()
						if cAddr != nil && n > 0 {
							packed, err := network.Sock5PackUDP(rAddr.IP.String(), rAddr.Port, buf[:n])
							if err == nil {
								_, _ = relayConn.WriteToUDP(packed, cAddr)
								udpSess.AddDatagram("", dec, 0, int64(n))
							}
						}
					}
				}(sess.addr, sess.conn, statsDecision(decision))
			}
			_, _ = sess.conn.WriteToUDP(payload, sess.addr)
			udpSess.AddDatagram(dstHost, statsDecision(decision), int64(len(payload)), 0)
		} else {
			// Upstream SPP cannot route IPv6
			if !s.cfg.EnableIPv6 && isIPv6Literal(dstHost) {
				continue
			}

			// Proxy via SPP
			sppUConn, sppRelayTarget, err := getOrCreateSPPUDPSession()
			if err != nil {
				loggo.Warn("[SOCKS5] SPP UDP session error for %s (%s): %v", targetAddr, realHost, err)
				// The association survives a failed datagram, so record the
				// error without closing the statistics session.
				s.cfg.Stats.RecordError("socks5", dstHost, "SPP UDP session for %s: %v", targetAddr, err)
				continue
			}
			if _, err := sppUConn.WriteToUDP(pktBuf[:n], sppRelayTarget); err == nil {
				udpSess.AddDatagram(dstHost, stats.DecisionProxy, int64(len(payload)), 0)
			}
		}
	}
}

// defaultIdleTimeout closes proxied connections whose peer vanished without a
// FIN/RST and that transfer no data for this long.
const defaultIdleTimeout = 5 * time.Minute

// relayCopyBufferSize matches io.Copy's default transfer buffer.
const relayCopyBufferSize = 32 * 1024

// relayBufPool recycles the per-direction copy buffers. Two of them are
// needed for every relayed connection, which made them the largest
// allocation on the forwarding path by a wide margin (about 60% of all
// bytes a busy relay allocates) and a steady source of GC work at
// connection churn. Pooling keeps one set per in-flight connection instead
// of one set per connection ever seen.
//
// The pool stores *[relayCopyBufferSize]byte rather than []byte so taking a
// buffer does not allocate a slice header.
var relayBufPool = sync.Pool{
	New: func() any { return new([relayCopyBufferSize]byte) },
}

// statsDecision converts a router decision into a statistics decision.
func statsDecision(d router.RouteDecision) stats.Decision {
	if d == router.Direct {
		return stats.DecisionDirect
	}
	return stats.DecisionProxy
}

// relayStreams copies both ways. a is the client side and b the target side:
// bytes read from a are upload (client -> target) and bytes read from b are
// download (target -> client). sess may be nil, in which case no traffic is
// accounted.
func relayStreams(a, b net.Conn, idle time.Duration, sess *stats.Session) {
	// lastActivity is shared by both directions, so a one-way transfer (e.g. a
	// long download with no upstream data) keeps the connection alive.
	lastActivity := time.Now().UnixNano()
	touch := func(t int64) { atomic.StoreInt64(&lastActivity, t) }

	done := make(chan struct{})
	watchdog := func() {
		interval := idle / 4
		if interval < time.Second {
			interval = idle // tiny test timeouts: check once per idle period
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if d := time.Since(time.Unix(0, atomic.LoadInt64(&lastActivity))); d > idle {
					// Closing the sockets unblocks the blocked Read calls.
					_ = a.Close()
					_ = b.Close()
					return
				}
			}
		}
	}
	go watchdog()

	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst, src net.Conn, count func(int64)) {
		defer wg.Done()
		bufp := relayBufPool.Get().(*[relayCopyBufferSize]byte)
		defer relayBufPool.Put(bufp)
		buf := bufp[:]
		for {
			n, rerr := src.Read(buf)
			if n > 0 {
				// One clock read serves both the idle watchdog and the
				// write deadline.
				now := time.Now()
				touch(now.UnixNano())
				count(int64(n))
				// A peer that stops reading must not pin this goroutine and
				// both sockets: without a write deadline the Write blocks
				// until the OS retransmit timeout (minutes), and the idle
				// watchdog cannot fire because data keeps arriving. Bound
				// every write by the idle window instead.
				_ = dst.SetWriteDeadline(now.Add(idle))
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if rerr != nil {
				break
			}
		}
		// Half-close the write side if the peer supports it so the peer sees EOF
		// without aborting in-flight packets or triggering TCP RST.
		if tcp, ok := dst.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		} else {
			_ = dst.Close()
		}
		// Give the opposite pipe a bounded grace period to complete reading/closing.
		grace := idle
		if grace > 15*time.Second {
			grace = 15 * time.Second
		}
		_ = src.SetReadDeadline(time.Now().Add(grace))
		_ = dst.SetReadDeadline(time.Now().Add(grace))
	}

	// a is the client side: reading b and writing a is target -> client.
	go pipe(a, b, sess.AddDownload)
	go pipe(b, a, sess.AddUpload)
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
	close(done)
}
