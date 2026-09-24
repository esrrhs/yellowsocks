package proxy

import (
	"bufio"
	"encoding/base64"
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

// HTTPConfig configures the HTTP proxy server
type HTTPConfig struct {
	ListenAddr string
	Username   string
	Password   string
	Router     *router.Router
	DNS        *dns.Server
	Upstream   UpstreamProvider
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

func (s *HTTPServer) handleClient(clientConn net.Conn) {
	defer common.CrashLog()
	defer clientConn.Close()

	_ = clientConn.SetDeadline(time.Now().Add(15 * time.Second))

	br := bufio.NewReader(clientConn)

	reqLine, err := br.ReadString('\n')
	if err != nil {
		return
	}

	reqLineTrimmed := strings.TrimRight(reqLine, "\r\n")
	parts := strings.Split(reqLineTrimmed, " ")
	if len(parts) < 3 {
		return
	}

	method := strings.ToUpper(parts[0])
	rawURI := parts[1]
	protoVer := parts[2]

	var rawHeaders []string
	var hostHeader string
	var proxyAuthHeader string

	for {
		headerLine, err := br.ReadString('\n')
		if err != nil {
			return
		}
		trimmed := strings.TrimRight(headerLine, "\r\n")
		if trimmed == "" {
			break
		}

		colonIdx := strings.IndexByte(trimmed, ':')
		if colonIdx > 0 {
			k := strings.TrimSpace(trimmed[:colonIdx])
			v := strings.TrimSpace(trimmed[colonIdx+1:])
			if strings.EqualFold(k, "Host") {
				hostHeader = v
			} else if strings.EqualFold(k, "Proxy-Authorization") {
				proxyAuthHeader = v
				continue
			} else if strings.EqualFold(k, "Proxy-Connection") {
				rawHeaders = append(rawHeaders, "Connection: "+v+"\r\n")
				continue
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
		s.handleConnect(clientConn, targetHost, targetPort)
	} else {
		s.handleStandardHTTP(clientConn, br, method, rawURI, protoVer, rawHeaders, targetHost, targetPort)
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
	return string(decoded) == s.cfg.Username+":"+s.cfg.Password
}

func (s *HTTPServer) parseTarget(method, rawURI, hostHeader string) (string, int) {
	if method == "CONNECT" {
		h, p, err := net.SplitHostPort(rawURI)
		if err == nil {
			port, _ := strconv.Atoi(p)
			return h, port
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
		port, _ = strconv.Atoi(p)
		host = h
	}
	return host, port
}

func (s *HTTPServer) routeDecision(host string, port int) (router.RouteDecision, string) {
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

func (s *HTTPServer) handleConnect(clientConn net.Conn, targetHost string, targetPort int) {
	decision, realHost := s.routeDecision(targetHost, targetPort)
	targetAddr := net.JoinHostPort(targetHost, strconv.Itoa(targetPort))
	ruleStr := "Proxy (SPP)"
	if decision == router.Direct {
		ruleStr = "Direct"
	}

	connID := clientConn.RemoteAddr().String() + "->" + targetAddr
	stats.Default.TrackConnection(connID, "http_connect", clientConn.RemoteAddr().String(), targetAddr, realHost, ruleStr)
	defer stats.Default.RemoveConnection(connID)

	loggo.Info("[HTTP Proxy] CONNECT %s -> %s (%s, Decision: %s)",
		clientConn.RemoteAddr(), targetAddr, realHost, ruleStr)

	if decision == router.Direct {
		targetConn, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
		if err != nil {
			loggo.Warn("[HTTP Proxy] Direct dial %s failed: %v", targetAddr, err)
			const http502 = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
			_, _ = clientConn.Write([]byte(http502))
			return
		}
		defer targetConn.Close()

		const http200 = "HTTP/1.1 200 Connection Established\r\n\r\n"
		if _, err := clientConn.Write([]byte(http200)); err != nil {
			return
		}
		relayStreams(clientConn, targetConn)
	} else {
		// Forward via SPP
		if s.cfg.Upstream == nil || s.cfg.Upstream.Socks5Addr() == "" {
			loggo.Error("[HTTP Proxy] No upstream SPP proxy available for proxy route to %s", targetAddr)
			const http502 = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
			_, _ = clientConn.Write([]byte(http502))
			return
		}

		sppTCPAddr, err := net.ResolveTCPAddr("tcp", s.cfg.Upstream.Socks5Addr())
		if err != nil {
			loggo.Error("[HTTP Proxy] Resolve SPP upstream failed: %v", err)
			const http502 = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
			_, _ = clientConn.Write([]byte(http502))
			return
		}

		sppConn, err := net.DialTCP("tcp", nil, sppTCPAddr)
		if err != nil {
			loggo.Error("[HTTP Proxy] Dial SPP upstream failed: %v", err)
			const http502 = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
			_, _ = clientConn.Write([]byte(http502))
			return
		}
		defer sppConn.Close()

		if err := network.Sock5Handshake(sppConn, 5000, "", ""); err != nil {
			const http502 = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
			_, _ = clientConn.Write([]byte(http502))
			return
		}

		if err := network.Sock5SetRequest(sppConn, targetHost, targetPort, 10000); err != nil {
			const http502 = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
			_, _ = clientConn.Write([]byte(http502))
			return
		}

		const http200 = "HTTP/1.1 200 Connection Established\r\n\r\n"
		if _, err := clientConn.Write([]byte(http200)); err != nil {
			return
		}
		relayStreams(clientConn, sppConn)
	}
}

func (s *HTTPServer) handleStandardHTTP(clientConn net.Conn, br *bufio.Reader, method, rawURI, protoVer string, rawHeaders []string, targetHost string, targetPort int) {
	decision, realHost := s.routeDecision(targetHost, targetPort)
	targetAddr := net.JoinHostPort(targetHost, strconv.Itoa(targetPort))
	ruleStr := "Proxy (SPP)"
	if decision == router.Direct {
		ruleStr = "Direct"
	}

	loggo.Info("[HTTP Proxy] %s %s -> %s (%s, Decision: %s)",
		method, rawURI, targetAddr, realHost, ruleStr)

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
	for _, h := range rawHeaders {
		reqBuf.WriteString(h)
	}
	reqBuf.WriteString("\r\n")

	var targetConn net.Conn
	var err error

	if decision == router.Direct {
		targetConn, err = net.DialTimeout("tcp", targetAddr, 10*time.Second)
	} else {
		if s.cfg.Upstream == nil || s.cfg.Upstream.Socks5Addr() == "" {
			const http502 = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
			_, _ = clientConn.Write([]byte(http502))
			return
		}
		sppTCPAddr, rErr := net.ResolveTCPAddr("tcp", s.cfg.Upstream.Socks5Addr())
		if rErr == nil {
			sppConn, dialErr := net.DialTCP("tcp", nil, sppTCPAddr)
			if dialErr == nil {
				if hsErr := network.Sock5Handshake(sppConn, 5000, "", ""); hsErr == nil {
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

	relayStreams(clientPrefixed, targetConn)
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
