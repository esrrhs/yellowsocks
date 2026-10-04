package dns

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/yellowsocks/core/router"
	"github.com/miekg/dns"
	"golang.org/x/net/proxy"
)

// DNSCacheEntry cache item
type DNSCacheEntry struct {
	Msg       *dns.Msg
	ExpiresAt time.Time
}

// Server DNS interception, DoH and DoT server
type Server struct {
	listenAddr    string
	dohListenAddr string
	dotListenAddr string
	tlsCertFile   string
	tlsKeyFile    string
	dohURL        string
	directDNS     string
	router        *router.Router
	socks5Addr    string
	socks5User    string
	socks5Pass    string
	udpServer     *dns.Server
	dotServer     *dns.Server
	httpServerDoH *http.Server
	cache         sync.Map // domain+type -> DNSCacheEntry
	ipToDomain    sync.Map // ip.String() -> domain
	fakeIPPool    *FakeIPPool
	enableFakeIP  bool
	enableIPv6    bool
	whitelist     sync.Map // proxy server domain -> struct{}, never Fake-IP
	realIP        sync.Map // proxy server domain -> pinnedAddr
	httpClientDoH *http.Client
	mu            sync.RWMutex
}

// Config DNS server configuration
type Config struct {
	ListenAddr    string // UDP listen address, e.g. 127.0.0.1:53 or :53
	DoHListenAddr string // TCP listen address for DoH, e.g. 127.0.0.1:8053 or :8053
	DoTListenAddr string // TCP-TLS listen address for DoT (RFC 7858), e.g. :853
	TLSCertFile   string // PEM certificate for DoT (required when DoTListenAddr is set)
	TLSKeyFile    string // PEM private key for DoT (required when DoTListenAddr is set)
	DoHURL        string // remote DoH resolver, e.g. https://1.1.1.1/dns-query
	DirectDNS     string // direct domestic/local DNS, e.g. 1.1.1.1:53 or 8.8.8.8:53
	Socks5Addr    string // upstream proxy socks5 address
	Socks5User    string
	Socks5Pass    string
	Router        *router.Router
	EnableFakeIP  bool // enable Fake-IP mode
	EnableIPv6    bool // answer AAAA for proxied domains; off returns empty AAAA
	FakeIPPool    *FakeIPPool
}

var processFakeIP = NewFakeIPPool()

// NewServer creates a new DNS interceptor and DoH/DoT server
func NewServer(cfg Config) (*Server, error) {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:53"
	}
	if cfg.DoHURL == "" {
		cfg.DoHURL = "https://1.1.1.1/dns-query"
	}
	if cfg.DirectDNS == "" {
		cfg.DirectDNS = "1.1.1.1:53"
	}
	if cfg.DoTListenAddr != "" && (cfg.TLSCertFile == "" || cfg.TLSKeyFile == "") {
		return nil, fmt.Errorf("dot_listen requires tls_cert and tls_key")
	}

	s := &Server{
		listenAddr:    cfg.ListenAddr,
		dohListenAddr: cfg.DoHListenAddr,
		dotListenAddr: cfg.DoTListenAddr,
		tlsCertFile:   cfg.TLSCertFile,
		tlsKeyFile:    cfg.TLSKeyFile,
		dohURL:        cfg.DoHURL,
		directDNS:     cfg.DirectDNS,
		router:        cfg.Router,
		socks5Addr:    cfg.Socks5Addr,
		socks5User:    cfg.Socks5User,
		socks5Pass:    cfg.Socks5Pass,
		enableFakeIP:  cfg.EnableFakeIP,
		enableIPv6:    cfg.EnableIPv6,
		fakeIPPool:    cfg.FakeIPPool,
	}
	if s.fakeIPPool == nil {
		s.fakeIPPool = processFakeIP
	}

	s.setupDoHClient()
	return s, nil
}

func (s *Server) setupDoHClient() {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: false},
	}

	if s.socks5Addr != "" {
		var auth *proxy.Auth
		if s.socks5User != "" {
			auth = &proxy.Auth{User: s.socks5User, Password: s.socks5Pass}
		}
		dialer, err := proxy.SOCKS5("tcp", s.socks5Addr, auth, proxy.Direct)
		if err == nil {
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
		} else {
			loggo.Error("[DNS] Failed to create socks5 dialer for DoH: %v", err)
		}
	}

	s.httpClientDoH = &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}
}

// UpdateSocks5Addr updates upstream socks5 address
func (s *Server) UpdateSocks5Addr(addr, user, pass string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.socks5Addr = addr
	s.socks5User = user
	s.socks5Pass = pass
	s.setupDoHClient()
}

// Start launches DNS UDP, DoH TCP and optional DoT (DNS-over-TLS) listeners
func (s *Server) Start() error {
	mux := dns.NewServeMux()
	mux.HandleFunc(".", s.handleDNSRequest)

	s.udpServer = &dns.Server{
		Addr:    s.listenAddr,
		Net:     "udp",
		Handler: mux,
	}

	pc, err := net.ListenPacket("udp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("listen udp %s: %w", s.listenAddr, err)
	}
	s.udpServer.PacketConn = pc
	if s.enableFakeIP && s.fakeIPPool != nil {
		s.fakeIPPool.Allocate("reserved.invalid")
		s.fakeIPPool.Allocate("dns.invalid")
	}
	loggo.Info("[DNS] Interceptor starting on UDP %s (Direct: %s, DoH Upstream: %s)", s.listenAddr, s.directDNS, s.dohURL)
	go func() {
		if err := s.udpServer.ActivateAndServe(); err != nil {
			loggo.Info("[DNS] UDP server stopped: %v", err)
		}
	}()

	if s.dohListenAddr != "" {
		muxDoH := http.NewServeMux()
		muxDoH.HandleFunc("/dns-query", s.handleDoHHTTP)
		muxDoH.HandleFunc("/", s.handleDoHHTTP)

		s.httpServerDoH = &http.Server{
			Addr:    s.dohListenAddr,
			Handler: muxDoH,
		}

		loggo.Info("[DNS] DoH server starting on TCP %s (/dns-query)", s.dohListenAddr)
		go func() {
			if err := s.httpServerDoH.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				loggo.Error("[DNS] DoH ListenAndServe error: %v", err)
			}
		}()
	}

	if s.dotListenAddr != "" {
		cert, err := tls.LoadX509KeyPair(s.tlsCertFile, s.tlsKeyFile)
		if err != nil {
			return fmt.Errorf("load DoT TLS certificate: %w", err)
		}
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			// RFC 7858 DoT ALPN
			NextProtos: []string{"dot"},
		}
		s.dotServer = &dns.Server{
			Addr:      s.dotListenAddr,
			Net:       "tcp-tls",
			TLSConfig: tlsConfig,
			Handler:   mux,
		}
		loggo.Info("[DNS] DoT server starting on TCP-TLS %s (cert=%s)", s.dotListenAddr, s.tlsCertFile)
		go func() {
			if err := s.dotServer.ListenAndServe(); err != nil {
				loggo.Error("[DNS] DoT ListenAndServe error: %v", err)
			}
		}()
	}

	return nil
}

// Stop stops DNS, DoH and DoT servers
func (s *Server) Stop() error {
	var firstErr error
	if s.udpServer != nil && s.udpServer.PacketConn != nil {
		// Shutdown() ignores a PacketConn when ActivateAndServe has not marked
		// the server started yet, which would leak the UDP listen port.
		if err := s.udpServer.PacketConn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.udpServer != nil {
		if err := s.udpServer.Shutdown(); err != nil && firstErr == nil && !strings.Contains(err.Error(), "not started") {
			firstErr = err
		}
	}
	if s.httpServerDoH != nil {
		if err := s.httpServerDoH.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.dotServer != nil {
		if err := s.dotServer.Shutdown(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *Server) handleDNSRequest(w dns.ResponseWriter, r *dns.Msg) {
	resp, err := s.ResolveMsg(r)
	if err != nil || resp == nil {
		dns.HandleFailed(w, r)
		return
	}
	resp.Id = r.Id
	_ = w.WriteMsg(resp)
}

func (s *Server) handleDoHHTTP(w http.ResponseWriter, r *http.Request) {
	var rawMsg []byte
	var err error

	switch r.Method {
	case http.MethodGet:
		dnsParam := r.URL.Query().Get("dns")
		if dnsParam == "" {
			http.Error(w, "missing dns query parameter", http.StatusBadRequest)
			return
		}
		rawMsg, err = base64.RawURLEncoding.DecodeString(dnsParam)
		if err != nil {
			rawMsg, err = base64.URLEncoding.DecodeString(dnsParam)
			if err != nil {
				http.Error(w, "invalid base64url dns parameter", http.StatusBadRequest)
				return
			}
		}
	case http.MethodPost:
		rawMsg, err = io.ReadAll(r.Body)
		if err != nil || len(rawMsg) == 0 {
			http.Error(w, "empty or invalid request body", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	reqMsg := new(dns.Msg)
	if err := reqMsg.Unpack(rawMsg); err != nil {
		http.Error(w, "failed to unpack dns message", http.StatusBadRequest)
		return
	}

	respMsg, err := s.ResolveMsg(reqMsg)
	if err != nil || respMsg == nil {
		http.Error(w, "dns resolution failed", http.StatusBadGateway)
		return
	}

	packed, err := respMsg.Pack()
	if err != nil {
		http.Error(w, "failed to pack dns response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/dns-message")
	w.Header().Set("Cache-Control", "max-age=60")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(packed)
}

// ResolveMsg resolves a DNS query message using caching, smart routing, and upstream DoH/Direct
func (s *Server) ResolveMsg(r *dns.Msg) (*dns.Msg, error) {
	if len(r.Question) == 0 {
		return nil, errors.New("empty question")
	}

	q := r.Question[0]
	qName := strings.ToLower(strings.Trim(q.Name, "."))
	cacheKey := fmt.Sprintf("%s_%d_%d", qName, q.Qtype, q.Qclass)

	// 1. Check cache
	if val, ok := s.cache.Load(cacheKey); ok {
		entry := val.(DNSCacheEntry)
		if time.Now().Before(entry.ExpiresAt) {
			resp := entry.Msg.Copy()
			resp.Id = r.Id
			return resp, nil
		}
		s.cache.Delete(cacheKey)
	}

	// 2. Routing resolution
	var resp *dns.Msg
	var err error

	if s.whitelisted(qName) {
		// Proxy endpoints stay on the real resolver. A Fake-IP here loops the tunnel into itself.
		if ip := s.pinned(qName, q.Qtype); ip != nil {
			loggo.Info("[DNS] %s -> %s (whitelist)", qName, ip)
			return ipReply(r, q.Qtype, ip), nil
		}
		if q.Qtype != dns.TypeA && q.Qtype != dns.TypeAAAA {
			return emptyReply(r), nil
		}
		resp, err = s.resolveDirect(r)
		if err != nil || resp == nil {
			return nil, err
		}
		resp.Id = r.Id
		return resp, nil
	}

	isDirect := s.router != nil && s.router.ShouldDirectDomain(qName)
	if isDirect {
		resp, err = s.resolveDirect(r)
	} else if !s.enableIPv6 && q.Qtype == dns.TypeAAAA {
		// Upstream has no IPv6 route. An empty AAAA makes clients use the A record.
		resp = emptyReply(r)
	} else if s.enableFakeIP && q.Qtype == dns.TypeA {
		fakeIP := s.fakeIPPool.Allocate(qName)
		resp = new(dns.Msg)
		resp.SetReply(r)
		rr := &dns.A{
			Hdr: dns.RR_Header{
				Name:   q.Name,
				Rrtype: dns.TypeA,
				Class:  dns.ClassINET,
				Ttl:    600,
			},
			A: fakeIP,
		}
		resp.Answer = append(resp.Answer, rr)
		s.ipToDomain.Store(fakeIP.String(), qName)
		loggo.Info("[DNS] %s -> %s", qName, fakeIP)
	} else if s.enableFakeIP && q.Qtype != dns.TypeAAAA {
		// HTTPS / SVCB would block the client on an upstream lookup. Empty answer keeps the A record.
		resp = emptyReply(r)
	} else {
		resp, err = s.resolveDoH(r)
		if err != nil {
			loggo.Warn("[DNS] DoH failed for %s, falling back to direct DNS: %v", qName, err)
			resp, err = s.resolveDirect(r)
		}
	}

	// A missing AAAA must not fail the lookup. Clients fall back to the A answer.
	if q.Qtype == dns.TypeAAAA && (err != nil || resp == nil) {
		resp = emptyReply(r)
		err = nil
	}

	if err != nil || resp == nil {
		return nil, err
	}

	resp.Id = r.Id
	// 3. Cache response and mapping
	s.cacheAndRecordIP(qName, cacheKey, resp)
	return resp, nil
}

// WhitelistDomain keeps a proxy hostname out of the Fake-IP pool.
func (s *Server) WhitelistDomain(domain string) {
	domain = strings.ToLower(strings.Trim(domain, "."))
	if domain == "" || net.ParseIP(domain) != nil {
		return
	}
	s.whitelist.Store(domain, struct{}{})
}

func (s *Server) whitelisted(domain string) bool {
	_, ok := s.whitelist.Load(domain)
	return ok
}

// pinnedAddr holds the real addresses of a proxy endpoint, one per family.
type pinnedAddr struct {
	v4 net.IP
	v6 net.IP
}

// PinRealIP forces a whitelisted hostname to a real A or AAAA record.
func (s *Server) PinRealIP(domain string, ip net.IP) {
	domain = strings.ToLower(strings.Trim(domain, "."))
	if domain == "" || ip == nil || IsFakeIP(ip) {
		return
	}
	var cur pinnedAddr
	if v, ok := s.realIP.Load(domain); ok {
		cur = v.(pinnedAddr)
	}
	if v4 := ip.To4(); v4 != nil {
		cur.v4 = append(net.IP(nil), v4...)
	} else if v6 := ip.To16(); v6 != nil {
		cur.v6 = append(net.IP(nil), v6...)
	} else {
		return
	}
	s.WhitelistDomain(domain)
	s.realIP.Store(domain, cur)
}

func (s *Server) pinned(domain string, qtype uint16) net.IP {
	v, ok := s.realIP.Load(domain)
	if !ok {
		return nil
	}
	cur := v.(pinnedAddr)
	if qtype == dns.TypeAAAA {
		return cur.v6
	}
	if qtype == dns.TypeA {
		return cur.v4
	}
	return nil
}

func aReply(r *dns.Msg, ip net.IP) *dns.Msg {
	return ipReply(r, dns.TypeA, ip)
}

func ipReply(r *dns.Msg, qtype uint16, ip net.IP) *dns.Msg {
	resp := new(dns.Msg)
	resp.SetReply(r)
	hdr := dns.RR_Header{
		Name:   r.Question[0].Name,
		Rrtype: qtype,
		Class:  dns.ClassINET,
		Ttl:    60,
	}
	switch qtype {
	case dns.TypeAAAA:
		resp.Answer = append(resp.Answer, &dns.AAAA{Hdr: hdr, AAAA: ip})
	default:
		resp.Answer = append(resp.Answer, &dns.A{Hdr: hdr, A: ip})
	}
	return resp
}

func emptyReply(r *dns.Msg) *dns.Msg {
	resp := new(dns.Msg)
	resp.SetReply(r)
	return resp
}

// ResolvePublicA returns a real A record for display. Proxied names prefer DoH.
func (s *Server) ResolvePublicA(host string, direct bool) (string, error) {
	if !direct {
		q := new(dns.Msg)
		q.SetQuestion(dns.Fqdn(host), dns.TypeA)
		if in, err := s.resolveDoH(q); err == nil {
			for _, ans := range in.Answer {
				if a, ok := ans.(*dns.A); ok && a.A != nil && !IsFakeIP(a.A) {
					return a.A.String(), nil
				}
			}
		}
	}
	ip, err := ResolveDirectA(s.directDNS, host)
	if err != nil {
		return "", err
	}
	return ip.String(), nil
}

// ResolveDirectA looks up an A record at directDNS, not via the system resolver.
func ResolveDirectA(directDNS, host string) (net.IP, error) {
	if directDNS == "" {
		directDNS = "114.114.114.114:53"
	}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(host), dns.TypeA)
	c := &dns.Client{Net: "udp", Timeout: 3 * time.Second}
	in, _, err := c.Exchange(m, directDNS)
	if err != nil {
		return nil, err
	}
	for _, ans := range in.Answer {
		if a, ok := ans.(*dns.A); ok && a.A.To4() != nil && !IsFakeIP(a.A) {
			return append(net.IP(nil), a.A.To4()...), nil
		}
	}
	return nil, fmt.Errorf("no A record for %s", host)
}

func (s *Server) resolveDirect(r *dns.Msg) (*dns.Msg, error) {
	c := &dns.Client{Net: "udp", Timeout: 2 * time.Second}
	in, _, err := c.Exchange(r, s.directDNS)
	return in, err
}

func (s *Server) resolveDoH(r *dns.Msg) (*dns.Msg, error) {
	data, err := r.Pack()
	if err != nil {
		return nil, err
	}

	s.mu.RLock()
	client := s.httpClientDoH
	doh := s.dohURL
	s.mu.RUnlock()

	req, err := http.NewRequest("POST", doh, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("doh status error: %d", resp.StatusCode)
	}

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	in := new(dns.Msg)
	if err := in.Unpack(respData); err != nil {
		return nil, err
	}
	return in, nil
}

func (s *Server) cacheAndRecordIP(domain, key string, msg *dns.Msg) {
	if len(msg.Answer) == 0 {
		return
	}

	var minTTL uint32 = 300
	for _, ans := range msg.Answer {
		if ans.Header().Ttl < minTTL && ans.Header().Ttl > 0 {
			minTTL = ans.Header().Ttl
		}
		if a, ok := ans.(*dns.A); ok {
			s.ipToDomain.Store(a.A.String(), domain)
		}
		if aaaa, ok := ans.(*dns.AAAA); ok {
			s.ipToDomain.Store(aaaa.AAAA.String(), domain)
		}
	}

	s.cache.Store(key, DNSCacheEntry{
		Msg:       msg.Copy(),
		ExpiresAt: time.Now().Add(time.Duration(minTTL) * time.Second),
	})
}

// LookupDomainByIP reverse lookup domain by IP
func (s *Server) LookupDomainByIP(ip string) (string, bool) {
	if parsed := net.ParseIP(ip); parsed != nil {
		if v4 := parsed.To4(); v4 != nil {
			ip = v4.String()
		} else {
			ip = parsed.String()
		}
	}
	if s.fakeIPPool != nil {
		if d, ok := s.fakeIPPool.LookupDomainByIP(ip); ok {
			return d, true
		}
	}
	val, ok := s.ipToDomain.Load(ip)
	if !ok {
		return "", false
	}
	return val.(string), true
}

// UDPAddr returns the UDP DNS listen address (e.g. 127.0.0.1:53).
func (s *Server) UDPAddr() string {
	return s.listenAddr
}
