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
	"github.com/esrrhs/yellowsocks/core/stats"
	"github.com/miekg/dns"
	"golang.org/x/net/proxy"
)

// DNSCacheEntry cache item
type DNSCacheEntry struct {
	Msg       *dns.Msg
	ExpiresAt time.Time
	// StoredAt is when the answer entered the cache. It lets cache hits age
	// every RR TTL by the elapsed time instead of guessing from ExpiresAt.
	StoredAt time.Time
}

// ipMapping is a domain learned for an IP, with an expiry so the reverse map
// stays bounded on long-running processes.
type ipMapping struct {
	domain  string
	expires time.Time
}

// Server serves DNS, DoH and DoT with rule-based upstream selection.
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
	tcpServer     *dns.Server
	dotServer     *dns.Server
	httpServerDoH *http.Server
	// Listeners are created before Start returns so Start fails
	// synchronously on a bind error. Keep references because
	// Shutdown/Close only release a listener after the serving goroutine has
	// marked the server started; Stop force-closes them to cover that race
	// and a Start that failed mid-way.
	dnsTCPListener net.Listener
	dohListener    net.Listener
	dotListener    net.Listener
	cache          sync.Map // domain+type -> DNSCacheEntry
	ipToDomain     sync.Map // ip.String() -> ipMapping, learned from answered queries
	enableIPv6     bool
	httpClientDoH  *http.Client
	stats          *stats.Collector
	stopCh         chan struct{}
	stopOnce       sync.Once
	janitorOnce    sync.Once
}

// maxDNSMessageSize is the largest DNS wire message (RFC 1035: a 2-octet
// length field over TCP), used to bound DoH requests and upstream responses.
const maxDNSMessageSize = 65535

// cacheSweepInterval is how often expired cache and IP mappings are reclaimed.
var cacheSweepInterval = 2 * time.Minute

// ipMappingTTL bounds how long an IP keeps reverse-resolving to a domain.
var ipMappingTTL = 30 * time.Minute

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
	EnableIPv6    bool // answer AAAA for proxied domains; off returns empty AAAA
	// Stats counts queries, cache hits and failures. Nil disables accounting.
	Stats *stats.Collector
}

// NewServer creates a DNS/DoH/DoT server.
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
		enableIPv6:    cfg.EnableIPv6,
		stats:         cfg.Stats,
		stopCh:        make(chan struct{}),
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
		if err != nil {
			loggo.Error("[DNS] Failed to create socks5 dialer for DoH: %v", err)
		} else if cd, ok := dialer.(proxy.ContextDialer); ok {
			// Honor the request context so a stalled local SPP SOCKS5 cannot
			// hang resolution past the client timeout.
			transport.DialContext = cd.DialContext
		} else {
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
		}
	}

	s.httpClientDoH = &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}
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
	loggo.Info("[DNS] Server starting on UDP %s (Direct: %s, DoH Upstream: %s)", s.listenAddr, s.directDNS, s.dohURL)
	go func() {
		if err := s.udpServer.ActivateAndServe(); err != nil {
			loggo.Info("[DNS] UDP server stopped: %v", err)
		}
	}()

	// RFC 5966: also answer over TCP so clients that get a truncated (TC=1)
	// UDP response can retry instead of failing the lookup. Bind before
	// Start returns so a successful Start means the port is already
	// accepting connections.
	tcpListener, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		s.Stop()
		return fmt.Errorf("listen tcp %s: %w", s.listenAddr, err)
	}
	s.dnsTCPListener = tcpListener
	s.tcpServer = &dns.Server{
		Listener: tcpListener,
		Net:      "tcp",
		Handler:  mux,
	}
	go func() {
		if err := s.tcpServer.ActivateAndServe(); err != nil {
			loggo.Info("[DNS] TCP server stopped: %v", err)
		}
	}()

	s.startJanitor()

	if s.dohListenAddr != "" {
		muxDoH := http.NewServeMux()
		muxDoH.HandleFunc("/dns-query", s.handleDoHHTTP)
		muxDoH.HandleFunc("/", s.handleDoHHTTP)

		dohListener, err := net.Listen("tcp", s.dohListenAddr)
		if err != nil {
			s.Stop()
			return fmt.Errorf("listen DoH %s: %w", s.dohListenAddr, err)
		}
		s.dohListener = dohListener
		s.httpServerDoH = &http.Server{
			Addr: dohListener.Addr().String(),
			// A network-facing DoH listener must not let clients hold
			// connections open forever: without a header deadline a slow
			// sender (classic Slowloris) pins one goroutine per connection.
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
			Handler:           muxDoH,
		}

		loggo.Info("[DNS] DoH server starting on TCP %s (/dns-query)", s.dohListenAddr)
		go func() {
			if err := s.httpServerDoH.Serve(dohListener); err != nil && err != http.ErrServerClosed {
				loggo.Error("[DNS] DoH serve error: %v", err)
			}
		}()
	}

	if s.dotListenAddr != "" {
		cert, err := tls.LoadX509KeyPair(s.tlsCertFile, s.tlsKeyFile)
		if err != nil {
			s.Stop()
			return fmt.Errorf("load DoT TLS certificate: %w", err)
		}
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			// RFC 7858 DoT ALPN
			NextProtos: []string{"dot"},
		}
		dotListener, err := net.Listen("tcp", s.dotListenAddr)
		if err != nil {
			s.Stop()
			return fmt.Errorf("listen DoT %s: %w", s.dotListenAddr, err)
		}
		s.dotListener = dotListener
		s.dotServer = &dns.Server{
			Listener: tls.NewListener(dotListener, tlsConfig),
			Net:      "tcp-tls",
			Handler:  mux,
		}
		loggo.Info("[DNS] DoT server starting on TCP-TLS %s (cert=%s)", s.dotListenAddr, s.tlsCertFile)
		go func() {
			if err := s.dotServer.ActivateAndServe(); err != nil {
				loggo.Error("[DNS] DoT serve error: %v", err)
			}
		}()
	}

	return nil
}

// Stop stops DNS, DoH and DoT servers. It is idempotent and safe to call
// after a Start that failed mid-way (Start itself releases what it bound).
func (s *Server) Stop() error {
	var stopErr error
	s.stopOnce.Do(func() {
		close(s.stopCh)
		stopErr = s.shutdownListeners()
	})
	return stopErr
}

func (s *Server) shutdownListeners() error {
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
	if s.tcpServer != nil {
		if err := s.tcpServer.Shutdown(); err != nil && firstErr == nil && !strings.Contains(err.Error(), "not started") {
			firstErr = err
		}
	}
	if s.dnsTCPListener != nil {
		// Covers the window before ActivateAndServe marked the server started.
		_ = s.dnsTCPListener.Close()
	}
	if s.httpServerDoH != nil {
		if err := s.httpServerDoH.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.dohListener != nil {
		_ = s.dohListener.Close()
	}
	if s.dotServer != nil {
		if err := s.dotServer.Shutdown(); err != nil && firstErr == nil && !strings.Contains(err.Error(), "not started") {
			firstErr = err
		}
	}
	if s.dotListener != nil {
		_ = s.dotListener.Close()
	}
	// Release keep-alive connections the upstream DoH client pooled.
	if t, ok := s.httpClientDoH.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
	return firstErr
}

// startJanitor launches the background reaper for expired DNS state.
func (s *Server) startJanitor() {
	s.janitorOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(cacheSweepInterval)
			defer ticker.Stop()
			for {
				select {
				case <-s.stopCh:
					return
				case <-ticker.C:
					s.sweepExpired()
				}
			}
		}()
	})
}

// sweepExpired removes stale cache entries and IP mappings. It is also called
// directly by tests instead of waiting for the ticker.
func (s *Server) sweepExpired() {
	now := time.Now()
	s.cache.Range(func(k, v any) bool {
		if now.After(v.(DNSCacheEntry).ExpiresAt) {
			s.cache.Delete(k)
		}
		return true
	})
	s.ipToDomain.Range(func(k, v any) bool {
		if now.After(v.(ipMapping).expires) {
			s.ipToDomain.Delete(k)
		}
		return true
	})
}

func (s *Server) handleDNSRequest(w dns.ResponseWriter, r *dns.Msg) {
	// RFC 7766: a truncated (TC=1) reply is only meaningful over UDP, where
	// it prompts the client to retry over TCP. Over an inbound TCP session TC
	// is never valid, so upstream truncation must surface as SERVFAIL rather
	// than being forwarded verbatim.
	viaTCP := false
	if ra := w.RemoteAddr(); ra != nil && strings.HasPrefix(ra.Network(), "tcp") {
		viaTCP = true
	}
	resp, err := s.resolve(r, viaTCP)
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
		// RFC 8484 POST carries one bare DNS message (<= 65535 octets, no
		// TCP length prefix). Cap the body so a client cannot exhaust
		// memory by streaming an unbounded POST.
		r.Body = http.MaxBytesReader(w, r.Body, maxDNSMessageSize)
		rawMsg, err = io.ReadAll(r.Body)
		if err != nil || len(rawMsg) == 0 {
			http.Error(w, "empty, oversized or invalid request body", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// The GET path is bounded only by the HTTP header limit, so enforce the
	// RFC 8484 wire-message ceiling here for both methods. A larger blob is
	// never a valid DNS message.
	if len(rawMsg) == 0 || len(rawMsg) > maxDNSMessageSize {
		http.Error(w, "dns message size out of range", http.StatusBadRequest)
		return
	}

	reqMsg := new(dns.Msg)
	if err := reqMsg.Unpack(rawMsg); err != nil {
		http.Error(w, "failed to unpack dns message", http.StatusBadRequest)
		return
	}

	respMsg, err := s.resolve(reqMsg, true)
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
	w.Header().Set("Cache-Control", doHCacheControl(respMsg))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(packed)
}

// doHMaxAgeCap bounds how long an HTTP cache may keep a DoH answer even if
// the upstream advertises a multi-year TTL.
const doHMaxAgeCap = 24 * time.Hour

// doHCacheControl builds the RFC 8484 (section 5.1) Cache-Control directive:
// max-age is the minimum TTL across every RR in the message. A TTL of 0 must
// produce max-age=0 so resolvers do not cache a deliberately non-cacheable
// answer; a message without RRs (e.g. a synthetic empty AAAA) is no-store.
func doHCacheControl(m *dns.Msg) string {
	var minTTL uint32
	found := false
	forEachRR(m, func(rr dns.RR) {
		if ttl := rr.Header().Ttl; !found || ttl < minTTL {
			minTTL = ttl
			found = true
		}
	})
	if !found {
		return "no-store"
	}
	capped := uint64(minTTL)
	if maxSec := uint64(doHMaxAgeCap / time.Second); capped > maxSec {
		capped = maxSec
	}
	return fmt.Sprintf("max-age=%d", capped)
}

// forEachRR visits every real RR in all message sections. OPT/EDNS(0) is a
// hop-by-hop pseudo-record without a cacheable TTL and is skipped.
func forEachRR(m *dns.Msg, fn func(dns.RR)) {
	for _, rrs := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range rrs {
			if rr.Header().Rrtype == dns.TypeOPT {
				continue
			}
			fn(rr)
		}
	}
}

// ageRRsTTL subtracts elapsed seconds from every RR TTL, floored at zero.
// Used on cache hits so records leave the resolver aged by the time they
// spent here (RFC 1034 section 3.6 / RFC 2181 section 8).
func ageRRsTTL(m *dns.Msg, elapsedSecs uint32) {
	forEachRR(m, func(rr dns.RR) {
		if h := rr.Header(); h.Ttl > elapsedSecs {
			h.Ttl -= elapsedSecs
		} else {
			h.Ttl = 0
		}
	})
}

// ResolveMsg resolves a DNS query message using caching, smart routing, and upstream DoH/Direct
func (s *Server) ResolveMsg(r *dns.Msg) (*dns.Msg, error) {
	// Public entry point has no transport context; use UDP semantics, where a
	// TC=1 answer from a failing upstream is a valid hint to retry over TCP.
	return s.resolve(r, false)
}

func (s *Server) resolve(r *dns.Msg, viaTCP bool) (*dns.Msg, error) {
	if len(r.Question) == 0 {
		return nil, errors.New("empty question")
	}

	q := r.Question[0]
	qName := strings.ToLower(strings.Trim(q.Name, "."))
	cacheKey := fmt.Sprintf("%s_%d_%d", qName, q.Qtype, q.Qclass)

	// 1. Check cache
	if val, ok := s.cache.Load(cacheKey); ok {
		entry := val.(DNSCacheEntry)
		remaining := time.Until(entry.ExpiresAt)
		if remaining > 0 {
			s.stats.RecordDNS(qName, qtypeString(q.Qtype), "cache", nil)
			resp := entry.Msg.Copy()
			// A caching resolver must age RR TTLs by the time the record
			// spent in cache (RFC 1034 3.6, RFC 2181 8). Returning the
			// original upstream TTL on every hit makes a record expiring
			// here look fresh to the client, and inflates the DoH
			// Cache-Control max-age derived from these TTLs.
			ageRRsTTL(resp, uint32(time.Since(entry.StoredAt)/time.Second))
			resp.Id = r.Id
			return resp, nil
		}
		s.cache.Delete(cacheKey)
	}

	// 2. Routing resolution
	var resp *dns.Msg
	var err error
	// upstream records which path answered, for the statistics API. Every
	// query is recorded exactly once, whichever branch it takes.
	upstream := "doh"

	isDirect := s.router != nil && s.router.ShouldDirectDomain(qName)
	switch {
	case isDirect:
		upstream = "direct"
		resp, err = s.resolveDirect(r, viaTCP)
	case !s.enableIPv6 && q.Qtype == dns.TypeAAAA:
		// Upstream has no IPv6 route. An empty AAAA makes clients use the A record.
		upstream = "synthetic"
		resp = emptyReply(r)
	default:
		resp, err = s.resolveDoH(r)
		if err != nil {
			loggo.Warn("[DNS] DoH failed for %s, falling back to direct DNS: %v", qName, err)
			upstream = "direct"
			resp, err = s.resolveDirect(r, viaTCP)
		}
	}

	// A missing AAAA must not fail the lookup. Clients fall back to the A answer.
	if q.Qtype == dns.TypeAAAA && (err != nil || resp == nil) {
		upstream = "synthetic"
		resp = emptyReply(r)
		err = nil
	}

	if err != nil || resp == nil {
		s.stats.RecordDNS(qName, qtypeString(q.Qtype), upstream, err)
		return nil, err
	}

	s.stats.RecordDNS(qName, qtypeString(q.Qtype), upstream, nil)

	resp.Id = r.Id
	// 3. Cache response and mapping. TC is a hop-by-hop transport signal
	// (RFC 7766 4.1), not part of the answer: a truncated message cached here
	// could later be served to a TCP/DoH client, on which TC is invalid.
	if !resp.Truncated {
		s.cacheAndRecordIP(qName, cacheKey, resp)
	}
	return resp, nil
}

func emptyReply(r *dns.Msg) *dns.Msg {
	resp := new(dns.Msg)
	resp.SetReply(r)
	return resp
}

// qtypeString renders a query type for the statistics API.
func qtypeString(qtype uint16) string {
	if name, ok := dns.TypeToString[qtype]; ok {
		return name
	}
	return fmt.Sprintf("TYPE%d", qtype)
}

// responseMatchesQuery verifies that an upstream message actually answers the
// question we sent (RFC 5452 section 5): it must carry QR, the same opcode and
// an identical question tuple (name, type, class). Without this check a
// compromised or buggy resolver could answer a different question, and that
// answer would be cached under the key of the question we sent, poisoning
// later lookups. The DNS library only matches the message Id.
func responseMatchesQuery(req, resp *dns.Msg) bool {
	if resp == nil || !resp.Response || resp.Opcode != req.Opcode {
		return false
	}
	if len(resp.Question) != len(req.Question) {
		return false
	}
	for i := range req.Question {
		want, got := req.Question[i], resp.Question[i]
		if want.Qtype != got.Qtype || want.Qclass != got.Qclass {
			return false
		}
		if !strings.EqualFold(strings.TrimSuffix(want.Name, "."), strings.TrimSuffix(got.Name, ".")) {
			return false
		}
	}
	return true
}

func (s *Server) resolveDirect(r *dns.Msg, viaTCP bool) (*dns.Msg, error) {
	c := &dns.Client{Net: "udp", Timeout: 2 * time.Second}
	in, _, err := c.Exchange(r, s.directDNS)
	if err != nil {
		return in, err
	}
	if !responseMatchesQuery(r, in) {
		loggo.Warn("[DNS] direct upstream %s answered a different question than %d/%s; rejecting",
			s.directDNS, r.Question[0].Qtype, r.Question[0].Name)
		return nil, errors.New("direct upstream answered a different question")
	}
	// The upstream set TC: the answer did not fit in a UDP datagram. A
	// caching resolver must retry the same question over TCP (RFC 2181 9,
	// RFC 7766) instead of returning a truncated, unusable answer.
	if in.Truncated {
		tcpClient := &dns.Client{Net: "tcp", Timeout: 2 * time.Second}
		inTCP, _, tcpErr := tcpClient.Exchange(r, s.directDNS)
		if tcpErr == nil && inTCP != nil && responseMatchesQuery(r, inTCP) {
			return inTCP, nil
		}
		if tcpErr == nil && inTCP != nil {
			tcpErr = errors.New("tcp retry answered a different question")
		}
		loggo.Warn("[DNS] direct upstream %s answered truncated over UDP and TCP retry failed: %v",
			s.directDNS, tcpErr)
		if viaTCP {
			// A truncated reply over an inbound TCP/DoH session is never
			// valid (RFC 7766); fail the lookup so the caller answers
			// SERVFAIL/502 instead of forwarding the TC bit.
			if tcpErr == nil {
				tcpErr = errors.New("upstream returned no TCP response")
			}
			return in, fmt.Errorf("direct upstream truncated and TCP retry failed: %w", tcpErr)
		}
	}
	return in, nil
}

func (s *Server) resolveDoH(r *dns.Msg) (*dns.Msg, error) {
	data, err := r.Pack()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", s.dohURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")

	resp, err := s.httpClientDoH.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("doh status error: %d", resp.StatusCode)
	}

	// A DoH answer is one DNS message (<= 65535 octets). Read one byte past the
	// ceiling: LimitReader at the ceiling cannot tell an exactly-maximal
	// message from an oversized one, and dns.Msg.Unpack ignores trailing
	// bytes, so extra octets from a compromised or buggy resolver would be
	// silently truncated and accepted instead of rejected.
	respData, err := io.ReadAll(io.LimitReader(resp.Body, maxDNSMessageSize+1))
	if err != nil {
		return nil, err
	}
	if len(respData) > maxDNSMessageSize {
		return nil, fmt.Errorf("doh response exceeds %d-octet DNS message limit", maxDNSMessageSize)
	}

	in := new(dns.Msg)
	if err := in.Unpack(respData); err != nil {
		return nil, err
	}
	if !responseMatchesQuery(r, in) {
		return nil, errors.New("doh upstream answered a different question")
	}
	// RFC 8484 delivers the answer over a stream transport, which cannot
	// truncate: TC is a UDP-only signal (RFC 2181 9, RFC 7766 4.1). A TC=1
	// on a DoH response is meaningless, so it must not be forwarded to the
	// client (same rule as the direct path) and must not stop an otherwise
	// valid, question-matching answer from being cached.
	if in.Truncated {
		loggo.Warn("[DNS] DoH upstream %s set TC on a stream answer for %s; clearing the bit",
			s.dohURL, r.Question[0].Name)
		in.Truncated = false
	}
	return in, nil
}

func (s *Server) cacheAndRecordIP(domain, key string, msg *dns.Msg) {
	// Only a NOERROR answer is a positive result. Servers may return a
	// partial Answer section alongside an error code (SERVFAIL, REFUSED,
	// ...); caching it would serve the failure as if it were an answer and
	// pin its IPs in the reverse map. Failure responses still reach the
	// client untouched, they just never enter the cache.
	if msg.Rcode != dns.RcodeSuccess || len(msg.Answer) == 0 {
		return
	}

	// RFC 2181: a TTL of 0 means the answer MUST NOT be cached. Recording it
	// with the floor TTL would pin a deliberately non-cacheable answer
	// (common for health-based/CDN records) for five minutes.
	var minTTL uint32 = 300
	for _, ans := range msg.Answer {
		ttl := ans.Header().Ttl
		if ttl == 0 {
			minTTL = 0
			break
		}
		if ttl < minTTL {
			minTTL = ttl
		}
	}
	now := time.Now()
	for _, ans := range msg.Answer {
		if a, ok := ans.(*dns.A); ok {
			s.ipToDomain.Store(a.A.String(), ipMapping{domain: domain, expires: now.Add(ipMappingTTL)})
		}
		if aaaa, ok := ans.(*dns.AAAA); ok {
			s.ipToDomain.Store(aaaa.AAAA.String(), ipMapping{domain: domain, expires: now.Add(ipMappingTTL)})
		}
	}

	if minTTL == 0 {
		// Drop any stale entry from a previous positive-TTL answer so the
		// authoritative no-cache directive takes effect immediately.
		s.cache.Delete(key)
		return
	}
	s.cache.Store(key, DNSCacheEntry{
		Msg:       msg.Copy(),
		StoredAt:  now,
		ExpiresAt: now.Add(time.Duration(minTTL) * time.Second),
	})
}

// LookupDomainByIP recovers a domain previously learned from an answered DNS
// query. It is used when clients connect to an IP literal.
func (s *Server) LookupDomainByIP(ip string) (string, bool) {
	if parsed := net.ParseIP(ip); parsed != nil {
		if v4 := parsed.To4(); v4 != nil {
			ip = v4.String()
		} else {
			ip = parsed.String()
		}
	}
	val, ok := s.ipToDomain.Load(ip)
	if !ok {
		return "", false
	}
	m := val.(ipMapping)
	if time.Now().After(m.expires) {
		s.ipToDomain.Delete(ip)
		return "", false
	}
	return m.domain, true
}
