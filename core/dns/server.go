package dns

import (
	"bytes"
	"context"
	"crypto/tls"
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

// Server DNS interception server
type Server struct {
	listenAddr    string
	dohURL        string
	directDNS     string
	router        *router.Router
	socks5Addr    string
	udpServer     *dns.Server
	cache         sync.Map // domain+type -> DNSCacheEntry
	ipToDomain    sync.Map // ip.String() -> domain
	fakeIPPool    *FakeIPPool
	enableFakeIP  bool
	httpClientDoH *http.Client
	mu            sync.RWMutex
}

// Config DNS server configuration
type Config struct {
	ListenAddr   string // listen address, e.g. 127.0.0.1:53 or 10.255.0.1:53
	DoHURL       string // remote DoH resolver, e.g. https://1.1.1.1/dns-query
	DirectDNS    string // direct domestic/local DNS, e.g. 1.1.1.1:53 or 8.8.8.8:53
	Socks5Addr   string // upstream proxy socks5 address
	Router       *router.Router
	EnableFakeIP bool // enable Fake-IP mode
}

// NewServer creates a new DNS interceptor
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

	s := &Server{
		listenAddr:   cfg.ListenAddr,
		dohURL:       cfg.DoHURL,
		directDNS:    cfg.DirectDNS,
		router:       cfg.Router,
		socks5Addr:   cfg.Socks5Addr,
		enableFakeIP: cfg.EnableFakeIP,
		fakeIPPool:   NewFakeIPPool(),
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

// Start launches DNS listener
func (s *Server) Start() error {
	mux := dns.NewServeMux()
	mux.HandleFunc(".", s.handleDNSRequest)

	s.udpServer = &dns.Server{
		Addr:    s.listenAddr,
		Net:     "udp",
		Handler: mux,
	}

	loggo.Info("[DNS] Interceptor starting on UDP %s (Direct: %s, DoH: %s)", s.listenAddr, s.directDNS, s.dohURL)
	go func() {
		if err := s.udpServer.ListenAndServe(); err != nil {
			loggo.Error("[DNS] ListenAndServe failed: %v", err)
		}
	}()
	return nil
}

// Stop stops DNS listener
func (s *Server) Stop() error {
	if s.udpServer != nil {
		return s.udpServer.Shutdown()
	}
	return nil
}

func (s *Server) handleDNSRequest(w dns.ResponseWriter, r *dns.Msg) {
	if len(r.Question) == 0 {
		return
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
			_ = w.WriteMsg(resp)
			return
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
		dns.HandleFailed(w, r)
		return
	}

	resp.Id = r.Id
	_ = w.WriteMsg(resp)

	// 3. Cache response and mapping
	s.cacheAndRecordIP(qName, cacheKey, resp)
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
