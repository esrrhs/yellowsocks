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
	"github.com/miekg/dns"
	"golang.org/x/net/proxy"
	"github.com/esrrhs/yellowsocks/core/router"
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
	udpServer     *dns.Server
	dotServer     *dns.Server
	httpServerDoH *http.Server
	cache         sync.Map // domain+type -> DNSCacheEntry
	ipToDomain    sync.Map // ip.String() -> domain
	fakeIPPool    *FakeIPPool
	enableFakeIP  bool
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
	Router        *router.Router
	EnableFakeIP  bool // enable Fake-IP mode
}

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
		enableFakeIP:  cfg.EnableFakeIP,
		fakeIPPool:    NewFakeIPPool(),
	}

	s.setupDoHClient()
	return s, nil
}

func (s *Server) setupDoHClient() {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: false},
	}

	if s.socks5Addr != "" {
		dialer, err := proxy.SOCKS5("tcp", s.socks5Addr, nil, proxy.Direct)
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
func (s *Server) UpdateSocks5Addr(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.socks5Addr = addr
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

	loggo.Info("[DNS] Interceptor starting on UDP %s (Direct: %s, DoH Upstream: %s)", s.listenAddr, s.directDNS, s.dohURL)
	go func() {
		if err := s.udpServer.ListenAndServe(); err != nil {
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
			// RFC 7858 / Android Private DNS ALPN
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
	if s.udpServer != nil {
		if err := s.udpServer.Shutdown(); err != nil && firstErr == nil {
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

	isDirect := s.router != nil && s.router.ShouldDirectDomain(qName)
	if isDirect {
		resp, err = s.resolveDirect(r)
	} else if s.enableFakeIP && q.Qtype == dns.TypeA {
		fakeIP := s.fakeIPPool.Allocate(qName)
		resp = new(dns.Msg)
		resp.SetReply(r)
		rr := &dns.A{
			Hdr: dns.RR_Header{
				Name:   q.Name,
				Rrtype: dns.TypeA,
				Class:  dns.ClassINET,
				Ttl:    60,
			},
			A: fakeIP,
		}
		resp.Answer = append(resp.Answer, rr)
		s.ipToDomain.Store(fakeIP.String(), qName)
	} else {
		resp, err = s.resolveDoH(r)
		if err != nil {
			loggo.Warn("[DNS] DoH failed for %s, falling back to direct DNS: %v", qName, err)
			resp, err = s.resolveDirect(r)
		}
	}

	if err != nil || resp == nil {
		return nil, err
	}

	resp.Id = r.Id
	// 3. Cache response and mapping
	s.cacheAndRecordIP(qName, cacheKey, resp)
	return resp, nil
}

func (s *Server) resolveDirect(r *dns.Msg) (*dns.Msg, error) {
	c := new(dns.Client)
	c.Timeout = 2 * time.Second
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
