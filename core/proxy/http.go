package proxy

import (
	"bufio"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/gohome/network"
	"github.com/esrrhs/yellowsocks/core/dns"
	"github.com/esrrhs/yellowsocks/core/router"
	"github.com/esrrhs/yellowsocks/core/stats"
)

// maxHTTPHeaderBytes bounds the combined size of the request line and the
// header block, matching net/http's DefaultMaxHeaderBytes. The parser used to
// accumulate lines without a cap, so a fast client could grow proxy memory
// without limit (the read deadline only protects against slow sends).
const maxHTTPHeaderBytes = 1 << 20

var errHTTPHeaderTooLarge = errors.New("http request head exceeds the size limit")

// readLimitedLine reads a single LF-terminated line from br while charging
// every byte against remaining (the shared budget for the request line and
// the whole header block). Unlike bufio.Reader.ReadString, it never buffers
// more than the remaining budget when a line is overlong.
func readLimitedLine(br *bufio.Reader, remaining *int) (string, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		n := len(chunk)
		if n > *remaining {
			return "", errHTTPHeaderTooLarge
		}
		*remaining -= n
		line = append(line, chunk...)
		if err == nil {
			return string(line), nil
		}
		if err != bufio.ErrBufferFull {
			return string(line), err
		}
	}
}

// HTTPConfig configures the HTTP proxy server
type HTTPConfig struct {
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

// HTTPServer provides inbound HTTP and HTTPS (CONNECT) proxy with smart routing
type HTTPServer struct {
	cfg      HTTPConfig
	listener net.Listener
	mu       sync.Mutex
	closed   bool
	stopCh   chan struct{}
}

// NewHTTPServer creates a new HTTP/HTTPS proxy server instance
func NewHTTPServer(cfg HTTPConfig) *HTTPServer {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:8080"
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultIdleTimeout
	}
	return &HTTPServer{
		cfg:    cfg,
		stopCh: make(chan struct{}),
	}
}

// Start begins listening on the configured TCP address
func (s *HTTPServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.listener != nil {
		return nil
	}

	l, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("http proxy listen failed: %w", err)
	}
	s.listener = l
	s.closed = false

	loggo.Info("[HTTP Proxy] Server listening on %s (HTTP GET/POST + HTTPS CONNECT)", s.cfg.ListenAddr)

	go s.acceptLoop(l)
	return nil
}

// Addr returns the actual listening address
func (s *HTTPServer) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Addr()
	}
	return nil
}

// Stop closes the server and terminates active sessions
func (s *HTTPServer) Stop() error {
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

func (s *HTTPServer) acceptLoop(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
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

func (s *HTTPServer) handleClient(clientConn net.Conn) {
	defer common.CrashLog()
	defer clientConn.Close()

	_ = clientConn.SetDeadline(time.Now().Add(15 * time.Second))

	br := bufio.NewReader(clientConn)

	// Shared byte budget for the request line and all header lines.
	headerBudget := maxHTTPHeaderBytes
	reqLine, err := readLimitedLine(br, &headerBudget)
	if err != nil {
		if errors.Is(err, errHTTPHeaderTooLarge) {
			const http400 = "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"
			_, _ = clientConn.Write([]byte(http400))
		}
		return
	}

	reqLineTrimmed := strings.TrimRight(reqLine, "\r\n")
	// RFC 7230 3.1.1: the request line is exactly three SP-separated tokens
	// of visible ASCII. The old len(parts) >= 3 check let "GET  /  HTTP/1.1"
	// through with an empty method and an empty request target, and the proxy
	// then forwarded that corrupted line to the origin. Reject anything that
	// is not exactly three non-empty printable tokens.
	parts := strings.Split(reqLineTrimmed, " ")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" ||
		!isPrintableASCII(reqLineTrimmed) || !strings.HasPrefix(parts[2], "HTTP/") {
		const http400 = "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"
		_, _ = clientConn.Write([]byte(http400))
		return
	}

	method := strings.ToUpper(parts[0])
	rawURI := parts[1]
	protoVer := parts[2]

	var rawHeaders []string
	var hostHeader string
	hostSeen := false
	var proxyAuthHeader string

	for {
		headerLine, err := readLimitedLine(br, &headerBudget)
		if err != nil {
			if errors.Is(err, errHTTPHeaderTooLarge) {
				const http400 = "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"
				_, _ = clientConn.Write([]byte(http400))
			}
			return
		}
		trimmed := strings.TrimRight(headerLine, "\r\n")
		if trimmed == "" {
			break
		}
		// A bare CR (or any other C0 control except the HTAB RFC 7230 allows
		// inside values) is forwarded verbatim to the origin, where a naive
		// parser reads it as a line end and honors the injected header.
		if containsControl(trimmed) {
			const http400 = "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"
			_, _ = clientConn.Write([]byte(http400))
			return
		}

		colonIdx := strings.IndexByte(trimmed, ':')
		if colonIdx > 0 {
			k := strings.TrimSpace(trimmed[:colonIdx])
			v := strings.TrimSpace(trimmed[colonIdx+1:])
			if strings.EqualFold(k, "Proxy-Authorization") {
				// Consumed locally for auth and never forwarded upstream.
				proxyAuthHeader = v
				continue
			}
			if strings.EqualFold(k, "Proxy-Connection") {
				rawHeaders = append(rawHeaders, "Connection: "+v+"\r\n")
				continue
			}
			if strings.EqualFold(k, "Host") {
				// Keep the value for target parsing but fall through to
				// append the original line: a proxy MUST forward Host
				// (RFC 7230 5.4); HTTP/1.1 vhost backends (nginx, ...)
				// reject the request with 400 when it is missing.
				hostHeader = v
				hostSeen = true
			}
		}
		rawHeaders = append(rawHeaders, trimmed+"\r\n")
	}

	// 1. Authenticate if required
	if !s.checkAuth(proxyAuthHeader) {
		const http407 = "HTTP/1.1 407 Proxy Authentication Required\r\n" +
			"Proxy-Authenticate: Basic realm=\"YellowSocks\"\r\n" +
			"Content-Length: 0\r\n\r\n"
		_, _ = clientConn.Write([]byte(http407))
		return
	}

	// 2. Parse target host & port
	targetHost, targetPort := s.parseTarget(method, rawURI, hostHeader)
	if targetHost == "" || targetPort <= 0 {
		const http400 = "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"
		_, _ = clientConn.Write([]byte(http400))
		return
	}

	_ = clientConn.SetDeadline(time.Time{})

	if method == "CONNECT" {
		s.handleConnect(clientConn, br, targetHost, targetPort)
	} else {
		s.handleStandardHTTP(clientConn, br, method, rawURI, protoVer, hostSeen, rawHeaders, targetHost, targetPort)
	}
}

func (s *HTTPServer) checkAuth(authHeader string) bool {
	if s.cfg.Username == "" && s.cfg.Password == "" {
		return true
	}
	if authHeader == "" {
		return false
	}
	const prefix = "Basic "
	if len(authHeader) < len(prefix) || !strings.EqualFold(authHeader[:len(prefix)], prefix) {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(authHeader[len(prefix):]))
	if err != nil {
		return false
	}
	// Constant-time comparison avoids leaking the credentials through timing.
	expected := []byte(s.cfg.Username + ":" + s.cfg.Password)
	return subtle.ConstantTimeCompare(decoded, expected) == 1
}

// isPrintableASCII reports whether s consists only of printable ASCII octets
// (0x20-0x7E, space included: it is the request-line token separator). Anything
// else in a request line is a broken client or a response-splitting attempt.
func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}

// containsControl reports whether s holds a C0 control octet other than the
// HTAB allowed inside header values, or DEL.
func containsControl(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\t') || c == 0x7f {
			return true
		}
	}
	return false
}

// parseTargetPort converts a port token, rejecting anything outside
// 1..65535. An out-of-range port is a malformed target: dialing it only
// burns a 502 round trip on a request the client got wrong.
func parseTargetPort(p string) (int, bool) {
	port, err := strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

func (s *HTTPServer) parseTarget(method, rawURI, hostHeader string) (string, int) {
	if method == "CONNECT" {
		h, p, err := net.SplitHostPort(rawURI)
		if err == nil {
			if port, ok := parseTargetPort(p); ok {
				return h, port
			}
			return "", 0
		}
		return rawURI, 443
	}

	// For GET/POST: URI may be absolute "http://example.com/path" or relative with Host header
	host := hostHeader
	port := 80
	if strings.HasPrefix(rawURI, "http://") {
		uriTrimmed := strings.TrimPrefix(rawURI, "http://")
		slashIdx := strings.IndexByte(uriTrimmed, '/')
		if slashIdx > 0 {
			host = uriTrimmed[:slashIdx]
		} else {
			host = uriTrimmed
		}
	}

	if h, p, err := net.SplitHostPort(host); err == nil {
		parsed, ok := parseTargetPort(p)
		if !ok {
			return "", 0
		}
		port = parsed
		host = h
	}
	return canonicalHost(host), port
}

func (s *HTTPServer) routeDecision(host string, port int) (router.RouteDecision, string) {
	realHost := host
	// Keep the literal IP even after a DNS reverse-lookup so CIDR/GeoIP rules
	// still apply when the host was learned from one of our own DNS answers.
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
func (s *HTTPServer) beginSession(p stats.Proto, client, host string, port int, decision router.RouteDecision) *stats.Session {
	sd := statsDecision(decision)
	node := ""
	if sd == stats.DecisionProxy && s.cfg.ActiveNode != nil {
		node = s.cfg.ActiveNode()
	}
	return s.cfg.Stats.Begin(p, client, host, port, sd, node)
}

func (s *HTTPServer) handleConnect(clientConn net.Conn, br *bufio.Reader, targetHost string, targetPort int) {
	targetHost = canonicalHost(targetHost)
	decision, realHost := s.routeDecision(targetHost, targetPort)
	targetAddr := joinDialAddr(targetHost, targetPort)
	ruleStr := "Proxy (SPP)"
	if decision == router.Direct {
		ruleStr = "Direct"
	}

	// Bytes the client pushed before reading our 200 reply may already be
	// buffered in br; the tunnel must drain them first or they are lost.
	tunnelConn := &prefixedReaderConn{Conn: clientConn, br: br}

	loggo.Info("[HTTP Proxy] CONNECT %s -> %s (%s, Decision: %s)",
		clientConn.RemoteAddr(), targetAddr, realHost, ruleStr)

	sess := s.beginSession(stats.ProtoHTTP, addrString(clientConn.RemoteAddr()), realHost, targetPort, decision)
	defer sess.End(nil)

	if decision == router.Direct {
		targetConn, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
		if err != nil {
			loggo.Warn("[HTTP Proxy] Direct dial %s failed: %v", targetAddr, err)
			sess.Fail(fmt.Errorf("direct dial %s: %w", targetAddr, err))
			const http502 = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
			_, _ = clientConn.Write([]byte(http502))
			return
		}
		defer targetConn.Close()

		const http200 = "HTTP/1.1 200 Connection Established\r\n\r\n"
		if _, err := clientConn.Write([]byte(http200)); err != nil {
			return
		}
		relayStreams(tunnelConn, targetConn, s.cfg.IdleTimeout, sess)
	} else {
		// Upstream SPP server cannot route IPv6
		if !s.cfg.EnableIPv6 && isIPv6Literal(targetHost) {
			loggo.Warn("[HTTP Proxy] Target is IPv6 literal %s, but IPv6 disabled (ipv6: false); returning 502 Bad Gateway", targetAddr)
			const http502 = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
			_, _ = clientConn.Write([]byte(http502))
			return
		}

		// Forward via SPP
		if s.cfg.Upstream == nil || s.cfg.Upstream.Socks5Addr() == "" {
			loggo.Error("[HTTP Proxy] No upstream SPP proxy available for proxy route to %s", targetAddr)
			sess.Fail(errors.New("no upstream SPP proxy available"))
			const http502 = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
			_, _ = clientConn.Write([]byte(http502))
			return
		}

		sppTCPAddr, err := net.ResolveTCPAddr("tcp", s.cfg.Upstream.Socks5Addr())
		if err != nil {
			loggo.Error("[HTTP Proxy] Resolve SPP upstream failed: %v", err)
			sess.Fail(fmt.Errorf("resolve SPP upstream: %w", err))
			const http502 = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
			_, _ = clientConn.Write([]byte(http502))
			return
		}

		sppConn, err := net.DialTCP("tcp", nil, sppTCPAddr)
		if err != nil {
			loggo.Error("[HTTP Proxy] Dial SPP upstream failed: %v", err)
			if rep, ok := s.cfg.Upstream.(interface{ ReportFailure(error) }); ok {
				rep.ReportFailure(err)
			}
			sess.Fail(fmt.Errorf("dial SPP upstream: %w", err))
			const http502 = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
			_, _ = clientConn.Write([]byte(http502))
			return
		}
		defer sppConn.Close()

		user, pass := "", ""
		if s.cfg.Upstream != nil {
			user, pass = s.cfg.Upstream.Socks5Auth()
		}
		if err := network.Sock5Handshake(sppConn, 5000, user, pass); err != nil {
			if rep, ok := s.cfg.Upstream.(interface{ ReportFailure(error) }); ok {
				rep.ReportFailure(err)
			}
			sess.Fail(fmt.Errorf("SPP upstream handshake: %w", err))
			const http502 = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
			_, _ = clientConn.Write([]byte(http502))
			return
		}

		if err := network.Sock5SetRequest(sppConn, targetHost, targetPort, 10000); err != nil {
			sess.Fail(fmt.Errorf("SPP upstream connect: %w", err))
			const http502 = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
			_, _ = clientConn.Write([]byte(http502))
			return
		}

		const http200 = "HTTP/1.1 200 Connection Established\r\n\r\n"
		if _, err := clientConn.Write([]byte(http200)); err != nil {
			return
		}
		relayStreams(tunnelConn, sppConn, s.cfg.IdleTimeout, sess)
	}
}

func (s *HTTPServer) handleStandardHTTP(clientConn net.Conn, br *bufio.Reader, method, rawURI, protoVer string, hostSeen bool, rawHeaders []string, targetHost string, targetPort int) {
	targetHost = canonicalHost(targetHost)
	decision, realHost := s.routeDecision(targetHost, targetPort)
	targetAddr := joinDialAddr(targetHost, targetPort)
	ruleStr := "Proxy (SPP)"
	if decision == router.Direct {
		ruleStr = "Direct"
	}

	loggo.Info("[HTTP Proxy] %s %s -> %s (%s, Decision: %s)",
		method, rawURI, targetAddr, realHost, ruleStr)

	sess := s.beginSession(stats.ProtoHTTP, addrString(clientConn.RemoteAddr()), realHost, targetPort, decision)
	defer sess.End(nil)

	// Rewrite absolute URI to relative origin-form
	relPath := rawURI
	if strings.HasPrefix(rawURI, "http://") {
		trimmed := strings.TrimPrefix(rawURI, "http://")
		slashIdx := strings.IndexByte(trimmed, '/')
		if slashIdx >= 0 {
			relPath = trimmed[slashIdx:]
		} else {
			relPath = "/"
		}
	}

	var reqBuf strings.Builder
	reqBuf.WriteString(fmt.Sprintf("%s %s %s\r\n", method, relPath, protoVer))
	if !hostSeen {
		// HTTP/1.0 clients may omit Host; synthesize one from the request
		// target so name-based backends still receive a valid request.
		syntheticHost := targetHost
		if targetPort != 80 {
			syntheticHost = net.JoinHostPort(targetHost, strconv.Itoa(targetPort))
		}
		reqBuf.WriteString("Host: " + syntheticHost + "\r\n")
	}
	for _, h := range rawHeaders {
		reqBuf.WriteString(h)
	}
	reqBuf.WriteString("\r\n")

	var targetConn net.Conn
	var err error

	if decision == router.Direct {
		targetConn, err = net.DialTimeout("tcp", targetAddr, 10*time.Second)
		if err != nil {
			sess.Fail(fmt.Errorf("direct dial %s: %w", targetAddr, err))
		}
	} else {
		// Upstream SPP server cannot route IPv6
		if !s.cfg.EnableIPv6 && isIPv6Literal(targetHost) {
			loggo.Warn("[HTTP Proxy] Target is IPv6 literal %s, but IPv6 disabled (ipv6: false); returning 502 Bad Gateway", targetAddr)
			const http502 = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
			_, _ = clientConn.Write([]byte(http502))
			return
		}

		if s.cfg.Upstream == nil || s.cfg.Upstream.Socks5Addr() == "" {
			const http502 = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
			_, _ = clientConn.Write([]byte(http502))
			return
		}
		sppTCPAddr, rErr := net.ResolveTCPAddr("tcp", s.cfg.Upstream.Socks5Addr())
		if rErr == nil {
			sppConn, dialErr := net.DialTCP("tcp", nil, sppTCPAddr)
			if dialErr == nil {
				user, pass := "", ""
				if s.cfg.Upstream != nil {
					user, pass = s.cfg.Upstream.Socks5Auth()
				}
				if hsErr := network.Sock5Handshake(sppConn, 5000, user, pass); hsErr == nil {
					if reqErr := network.Sock5SetRequest(sppConn, targetHost, targetPort, 10000); reqErr == nil {
						targetConn = sppConn
					}
				}
			}
			if targetConn == nil && sppConn != nil {
				_ = sppConn.Close()
			}
		}
		if targetConn == nil {
			err = fmt.Errorf("spp connection failed")
			sess.Fail(err)
		}
	}

	if err != nil || targetConn == nil {
		const http502 = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
		_, _ = clientConn.Write([]byte(http502))
		return
	}
	defer targetConn.Close()

	if _, err := targetConn.Write([]byte(reqBuf.String())); err != nil {
		return
	}

	// If client sent body (e.g. POST/PUT), we prefix the unread buffer from br
	clientPrefixed := &prefixedReaderConn{
		Conn: clientConn,
		br:   br,
	}

	relayStreams(clientPrefixed, targetConn, s.cfg.IdleTimeout, sess)
}

type prefixedReaderConn struct {
	net.Conn
	br *bufio.Reader
}

func (p *prefixedReaderConn) Read(b []byte) (int, error) {
	if p.br != nil && p.br.Buffered() > 0 {
		return p.br.Read(b)
	}
	return p.Conn.Read(b)
}
