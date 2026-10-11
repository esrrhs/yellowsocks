package dns

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/yellowsocks/core/router"
	"github.com/miekg/dns"
)

func writeTestTLSCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	certOut, err := os.Create(certFile)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode cert: %v", err)
	}
	_ = certOut.Close()

	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyOut, err := os.Create(keyFile)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		t.Fatalf("encode key: %v", err)
	}
	_ = keyOut.Close()
	return certFile, keyFile
}

// localDoHTTPServer stands up a deterministic RFC 8484 DoH endpoint so DNS
// tests stay fully offline: A queries return the mapped IP, other types empty.
func localDoHTTPServer(t *testing.T, aRecords map[string]string) *httptest.Server {
	t.Helper()
	return localDoHTTPServerWithTTL(t, aRecords, 60)
}

// localDoHTTPServerWithTTL is localDoHTTPServer with an explicit answer TTL,
// used to verify TTL-derived caching behavior.
func localDoHTTPServerWithTTL(t *testing.T, aRecords map[string]string, ttl uint32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw []byte
		switch r.Method {
		case http.MethodGet:
			b, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
			if err != nil {
				http.Error(w, "bad dns parameter", http.StatusBadRequest)
				return
			}
			raw = b
		case http.MethodPost:
			b, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			raw = b
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		req := new(dns.Msg)
		if err := req.Unpack(raw); err != nil || len(req.Question) == 0 {
			http.Error(w, "bad dns message", http.StatusBadRequest)
			return
		}
		resp := new(dns.Msg)
		resp.SetReply(req)
		q := req.Question[0]
		if q.Qtype == dns.TypeA {
			name := strings.ToLower(strings.TrimSuffix(q.Name, "."))
			if ipStr, ok := aRecords[name]; ok {
				resp.Answer = append(resp.Answer, &dns.A{
					Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl},
					A:   net.ParseIP(ipStr),
				})
			}
		}
		packed, err := resp.Pack()
		if err != nil {
			http.Error(w, "pack failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(packed)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDNSServerUDPAndDoH(t *testing.T) {
	// Pick free ports for UDP DNS and TCP DoH
	udpL, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen packet: %v", err)
	}
	udpAddr := udpL.LocalAddr().String()
	_ = udpL.Close()

	tcpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen tcp: %v", err)
	}
	dohAddr := tcpL.Addr().String()
	_ = tcpL.Close()

	dohStub := localDoHTTPServer(t, map[string]string{
		"google.com": "93.184.216.34",
		"github.com": "140.82.121.4",
	})

	srv, err := NewServer(Config{
		ListenAddr:    udpAddr,
		DoHListenAddr: dohAddr,
		DoHURL:        dohStub.URL + "/dns-query",
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop()

	// Wait briefly for listeners to start
	time.Sleep(100 * time.Millisecond)

	// 1. Test UDP DNS query
	c := new(dns.Client)
	c.Timeout = 2 * time.Second
	m := new(dns.Msg)
	m.SetQuestion("google.com.", dns.TypeA)

	resp, _, err := c.Exchange(m, udpAddr)
	if err != nil {
		t.Fatalf("UDP DNS exchange failed: %v", err)
	}
	if len(resp.Answer) == 0 {
		t.Fatalf("expected answer in UDP DNS response, got 0")
	}
	aRecord, ok := resp.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("expected A record, got %T", resp.Answer[0])
	}
	if !aRecord.A.Equal(net.ParseIP("93.184.216.34")) {
		t.Fatalf("expected stub A record 93.184.216.34, got %s", aRecord.A)
	}

	// Verify reverse lookup
	domain, ok := srv.LookupDomainByIP(aRecord.A.String())
	if !ok || domain != "google.com" {
		t.Errorf("expected domain google.com from reverse lookup of %s, got %s (ok=%v)", aRecord.A.String(), domain, ok)
	}

	// 2. Test TCP DoH query via HTTP GET (?dns=...)
	dohQueryMsg := new(dns.Msg)
	dohQueryMsg.SetQuestion("github.com.", dns.TypeA)
	packedQuery, err := dohQueryMsg.Pack()
	if err != nil {
		t.Fatalf("failed to pack doh query msg: %v", err)
	}

	b64Query := base64.RawURLEncoding.EncodeToString(packedQuery)
	getUrl := "http://" + dohAddr + "/dns-query?dns=" + b64Query
	httpResp, err := http.Get(getUrl)
	if err != nil {
		t.Fatalf("DoH GET request failed: %v", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 from DoH GET, got %d", httpResp.StatusCode)
	}
	if ct := httpResp.Header.Get("Content-Type"); ct != "application/dns-message" {
		t.Fatalf("expected Content-Type application/dns-message, got %s", ct)
	}
	getRespBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		t.Fatalf("failed to read DoH GET body: %v", err)
	}
	getRespMsg := new(dns.Msg)
	if err := getRespMsg.Unpack(getRespBody); err != nil {
		t.Fatalf("failed to unpack DoH GET response: %v", err)
	}
	if len(getRespMsg.Answer) == 0 {
		t.Fatalf("expected answer in DoH GET response, got 0")
	}

	// 3. Test TCP DoH query via HTTP POST (binary body)
	postUrl := "http://" + dohAddr + "/dns-query"
	postReq, err := http.NewRequest("POST", postUrl, bytes.NewReader(packedQuery))
	if err != nil {
		t.Fatalf("failed to create DoH POST request: %v", err)
	}
	postReq.Header.Set("Content-Type", "application/dns-message")
	postReq.Header.Set("Accept", "application/dns-message")

	postResp, err := http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatalf("DoH POST request failed: %v", err)
	}
	defer postResp.Body.Close()

	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 from DoH POST, got %d", postResp.StatusCode)
	}
	postRespBody, err := io.ReadAll(postResp.Body)
	if err != nil {
		t.Fatalf("failed to read DoH POST body: %v", err)
	}
	postRespMsg := new(dns.Msg)
	if err := postRespMsg.Unpack(postRespBody); err != nil {
		t.Fatalf("failed to unpack DoH POST response: %v", err)
	}
	if len(postRespMsg.Answer) == 0 {
		t.Fatalf("expected answer in DoH POST response, got 0")
	}
}

func TestDNSServerDoT(t *testing.T) {
	udpL, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	udpAddr := udpL.LocalAddr().String()
	_ = udpL.Close()

	tcpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	dotAddr := tcpL.Addr().String()
	_ = tcpL.Close()

	certFile, keyFile := writeTestTLSCert(t, t.TempDir())
	dohStub := localDoHTTPServer(t, map[string]string{"example.com": "23.215.0.138"})
	srv, err := NewServer(Config{
		ListenAddr:    udpAddr,
		DoTListenAddr: dotAddr,
		TLSCertFile:   certFile,
		TLSKeyFile:    keyFile,
		DoHURL:        dohStub.URL + "/dns-query",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Stop()
	time.Sleep(100 * time.Millisecond)

	c := &dns.Client{
		Net: "tcp-tls",
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "localhost",
			NextProtos:         []string{"dot"},
		},
		Timeout: 2 * time.Second,
	}
	m := new(dns.Msg)
	m.SetQuestion("example.com.", dns.TypeA)
	resp, _, err := c.Exchange(m, dotAddr)
	if err != nil {
		t.Fatalf("DoT exchange failed: %v", err)
	}
	if len(resp.Answer) == 0 {
		t.Fatalf("expected DoT answer, got 0")
	}
}

func TestNewServerDoTRequiresCert(t *testing.T) {
	_, err := NewServer(Config{DoTListenAddr: ":853"})
	if err == nil {
		t.Fatal("expected error when DoT enabled without cert/key")
	}
}

// When a later listener fails to bind, Start must return an error and Stop
// must release the UDP/TCP sockets already bound earlier in Start.
func TestStartFailureReleasesBoundListeners(t *testing.T) {
	udpL, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udpAddr := udpL.LocalAddr().String()
	_ = udpL.Close()

	// Occupy the DoH TCP port so Start fails at the synchronous DoH bind.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	srv, err := NewServer(Config{
		ListenAddr:    udpAddr,
		DoHListenAddr: occupied.Addr().String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err == nil {
		t.Fatal("expected Start to fail when DoH port is occupied")
	}
	if err := srv.Stop(); err != nil {
		t.Fatalf("stop after failed start: %v", err)
	}

	// The UDP listener acquired before the failure must be released.
	retryDeadline := time.Now().Add(2 * time.Second)
	for {
		pc, lerr := net.ListenPacket("udp", udpAddr)
		if lerr == nil {
			_ = pc.Close()
			return
		}
		if time.Now().After(retryDeadline) {
			t.Fatalf("UDP port %s still bound after failed Start: %v", udpAddr, lerr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAAAAEmptyWhenIPv6Disabled(t *testing.T) {
	// With EnableIPv6 off (the default) a proxied AAAA query is answered empty
	// so clients fall back to the A record.
	dohStub := localDoHTTPServer(t, nil)
	s, err := NewServer(Config{
		ListenAddr: "127.0.0.1:0",
		DoHURL:     dohStub.URL + "/dns-query",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := new(dns.Msg)
	m.SetQuestion("www.example.", dns.TypeAAAA)
	resp, err := s.ResolveMsg(m)
	if err != nil {
		t.Fatalf("AAAA resolve: %v", err)
	}
	if len(resp.Answer) != 0 {
		t.Fatalf("expected empty AAAA with IPv6 disabled, got %v", resp.Answer)
	}
}

func TestAAAAFromDirectDNS(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			req := new(dns.Msg)
			if err := req.Unpack(buf[:n]); err != nil || len(req.Question) == 0 {
				continue
			}
			resp := new(dns.Msg)
			resp.SetReply(req)
			q := req.Question[0]
			if q.Qtype == dns.TypeAAAA {
				resp.Answer = append(resp.Answer, &dns.AAAA{
					Hdr:  dns.RR_Header{Name: q.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 60},
					AAAA: net.ParseIP("2001:db8::53"),
				})
			}
			packed, err := resp.Pack()
			if err != nil {
				continue
			}
			_, _ = pc.WriteTo(packed, addr)
		}
	}()

	rt := router.NewRouter()
	defer rt.Close()
	rt.AddDirectDomain("v6.example")
	s, err := NewServer(Config{
		ListenAddr: "127.0.0.1:0",
		DirectDNS:  pc.LocalAddr().String(),
		DoHURL:     "http://127.0.0.1:1/dns-query",
		Router:     rt,
	})
	if err != nil {
		t.Fatal(err)
	}

	q := new(dns.Msg)
	q.SetQuestion("v6.example.", dns.TypeAAAA)
	resp, err := s.ResolveMsg(q)
	if err != nil || len(resp.Answer) != 1 {
		t.Fatalf("AAAA resolve: %v %v", err, resp)
	}
	aaaa, ok := resp.Answer[0].(*dns.AAAA)
	if !ok || !aaaa.AAAA.Equal(net.ParseIP("2001:db8::53")) {
		t.Fatalf("AAAA = %v", resp.Answer)
	}
	if host, ok := s.LookupDomainByIP("2001:db8:0:0:0:0:0:53"); !ok || host != "v6.example" {
		t.Fatalf("reverse IPv6 lookup: %q %v", host, ok)
	}
}

// freeUDPPort grabs and releases an ephemeral UDP port to reuse as an address.
func freeUDPPort(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	_ = pc.Close()
	return addr
}

// RFC 5966: the server must answer plain TCP DNS queries on the same port so
// clients retrying after a truncated (TC=1) UDP answer still resolve.
func TestDNSServerTCP(t *testing.T) {
	addr := freeUDPPort(t)
	dohStub := localDoHTTPServer(t, map[string]string{"tcp.example": "203.0.113.7"})

	srv, err := NewServer(Config{
		ListenAddr: addr,
		DoHURL:     dohStub.URL + "/dns-query",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(100 * time.Millisecond)

	c := &dns.Client{Net: "tcp", Timeout: 2 * time.Second}
	m := new(dns.Msg)
	m.SetQuestion("tcp.example.", dns.TypeA)
	resp, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("TCP DNS exchange failed: %v", err)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("expected 1 A record over TCP, got %d", len(resp.Answer))
	}
	a, ok := resp.Answer[0].(*dns.A)
	if !ok || !a.A.Equal(net.ParseIP("203.0.113.7")) {
		t.Fatalf("unexpected TCP answer: %v", resp.Answer)
	}
}

func TestDNSServerSweepExpired(t *testing.T) {
	srv, err := NewServer(Config{ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}

	srv.cache.Store("fresh_cache", DNSCacheEntry{Msg: new(dns.Msg), ExpiresAt: time.Now().Add(time.Minute)})
	srv.cache.Store("stale_cache", DNSCacheEntry{Msg: new(dns.Msg), ExpiresAt: time.Now().Add(-time.Minute)})
	srv.ipToDomain.Store("1.1.1.1", ipMapping{domain: "fresh.example", expires: time.Now().Add(time.Minute)})
	srv.ipToDomain.Store("2.2.2.2", ipMapping{domain: "stale.example", expires: time.Now().Add(-time.Minute)})

	srv.sweepExpired()

	if _, ok := srv.cache.Load("stale_cache"); ok {
		t.Fatal("stale cache entry should have been reaped")
	}
	if _, ok := srv.cache.Load("fresh_cache"); !ok {
		t.Fatal("fresh cache entry should be retained")
	}
	if d, ok := srv.LookupDomainByIP("1.1.1.1"); !ok || d != "fresh.example" {
		t.Fatalf("fresh mapping lost: %q %v", d, ok)
	}
	if _, ok := srv.LookupDomainByIP("2.2.2.2"); ok {
		t.Fatal("stale IP mapping should have been reaped")
	}
}

// Reserve a free loopback TCP port without keeping it, so a subsequent
// listener can bind it.
func reserveFreeTCPPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestDoHHTTPRejectsMalformedRequests(t *testing.T) {
	srv, err := NewServer(Config{
		ListenAddr:    "127.0.0.1:0",
		DoHListenAddr: reserveFreeTCPPort(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	base := "http://" + srv.httpServerDoH.Addr + "/dns-query"

	// Unsupported method -> 405.
	putReq, _ := http.NewRequest(http.MethodPut, base, strings.NewReader("x"))
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = putResp.Body.Close()
	if putResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("PUT status=%d, want 405", putResp.StatusCode)
	}

	// GET without dns parameter -> 400.
	respNoParam, err := http.Get(base)
	if err != nil {
		t.Fatal(err)
	}
	_ = respNoParam.Body.Close()
	if respNoParam.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET no-param status=%d, want 400", respNoParam.StatusCode)
	}

	// GET with undecodable base64 -> 400.
	respBadB64, err := http.Get(base + "?dns=%21%21%21")
	if err != nil {
		t.Fatal(err)
	}
	_ = respBadB64.Body.Close()
	if respBadB64.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET bad-base64 status=%d, want 400", respBadB64.StatusCode)
	}

	// Empty POST body -> 400.
	respEmpty, err := http.Post(base, "application/dns-message", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	_ = respEmpty.Body.Close()
	if respEmpty.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST empty status=%d, want 400", respEmpty.StatusCode)
	}
}

// RFC 8484 carries a single DNS message (<= 65535 octets). An oversized POST
// must be rejected instead of being fully buffered into memory.
func TestDoHHTTPRejectsOversizedPost(t *testing.T) {
	srv, err := NewServer(Config{
		ListenAddr:    "127.0.0.1:0",
		DoHListenAddr: reserveFreeTCPPort(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	resp, err := http.Post(
		"http://"+srv.httpServerDoH.Addr+"/dns-query",
		"application/dns-message",
		strings.NewReader(strings.Repeat("x", 70000)),
	)
	if err != nil {
		t.Fatalf("oversized post: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	// MaxBytesReader surfaces as 413 (or our explicit 400); either is a
	// rejection, and the body must never reach DNS unpacking.
	if resp.StatusCode != http.StatusRequestEntityTooLarge &&
		resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized POST status=%d, want 400 or 413", resp.StatusCode)
	}
}

func answerWithTTL(name string, ttl uint32) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(&dns.Msg{})
	m.Question = []dns.Question{{Name: name, Qtype: dns.TypeA, Qclass: dns.ClassINET}}
	m.Answer = append(m.Answer, &dns.A{
		Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl},
		A:   net.ParseIP("1.2.3.4"),
	})
	return m
}

// RFC 2181: an answer with TTL 0 must not be cached. The old code only
// considered positive TTLs and therefore cached it for the 300s floor.
func TestCacheSkipsZeroTTLAnswers(t *testing.T) {
	srv, err := NewServer(Config{ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	const key = "nocache.example_1_1"

	// A previously cached positive-TTL answer must be evicted once the
	// authoritative server returns TTL 0.
	srv.cache.Store(key, DNSCacheEntry{Msg: answerWithTTL("nocache.example.", 60), ExpiresAt: time.Now().Add(time.Minute)})
	srv.cacheAndRecordIP("nocache.example", key, answerWithTTL("nocache.example.", 0))

	if _, ok := srv.cache.Load(key); ok {
		t.Fatal("TTL=0 answer must not remain in cache")
	}
	// The learned IP mapping is still recorded for routing.
	if d, ok := srv.LookupDomainByIP("1.2.3.4"); !ok || d != "nocache.example" {
		t.Fatalf("IP mapping missing: %q %v", d, ok)
	}
}

func TestCacheStoresPositiveTTLAnswers(t *testing.T) {
	srv, err := NewServer(Config{ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	const key = "cached.example_1_1"
	srv.cacheAndRecordIP("cached.example", key, answerWithTTL("cached.example.", 60))
	entry, ok := srv.cache.Load(key)
	if !ok {
		t.Fatal("positive-TTL answer should be cached")
	}
	ttl := time.Until(entry.(DNSCacheEntry).ExpiresAt)
	if ttl < 55*time.Second || ttl > 60*time.Second {
		t.Fatalf("cache TTL=%v, want ~60s", ttl)
	}
}

// DoH goes through the local SPP SOCKS5. When that endpoint accepts the TCP
// connection but never completes the SOCKS5 handshake, the dial must abort
// when the request context expires instead of hanging resolution forever.
func TestDoHUpstreamDialerHonorsContext(t *testing.T) {
	blackhole, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan net.Conn, 64)
	// Cleanups run LIFO: stop accepting first, then release held conns.
	t.Cleanup(func() {
		for {
			select {
			case c := <-held:
				_ = c.Close()
			default:
				return
			}
		}
	})
	t.Cleanup(func() { _ = blackhole.Close() })
	go func() {
		for {
			c, err := blackhole.Accept()
			if err != nil {
				return
			}
			select {
			case held <- c:
			default:
				_ = c.Close()
			}
		}
	}()

	srv, err := NewServer(Config{
		ListenAddr: "127.0.0.1:0",
		Socks5Addr: blackhole.Addr().String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := srv.httpClientDoH.Transport.(*http.Transport)
	if !ok || tr.DialContext == nil {
		t.Fatal("DoH transport missing context-aware SOCKS5 dialer")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := tr.DialContext(ctx, "tcp", "example.com:443"); err == nil {
		t.Fatal("expected dial to fail when context expires")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("dial did not honor context deadline, took %v", d)
	}
}

// RFC 8484 section 5.1: the HTTP Cache-Control max-age must reflect the
// smallest RR TTL in the answer. In particular a TTL=0 answer must not be
// advertised as cacheable (the old code always sent max-age=60).
func TestDoHCacheControlFollowsAnswerTTL(t *testing.T) {
	cases := []struct {
		name string
		ttl  uint32
		want string
	}{
		{"zero ttl is uncacheable", 0, "max-age=0"},
		{"positive ttl passes through", 120, "max-age=120"},
		{"huge ttl is capped", uint32((48 * time.Hour) / time.Second), "max-age=86400"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := localDoHTTPServerWithTTL(t, map[string]string{"ttl.example": "198.51.100.9"}, tc.ttl)
			srv, err := NewServer(Config{
				ListenAddr:    "127.0.0.1:0",
				DoHListenAddr: reserveFreeTCPPort(t),
				DoHURL:        stub.URL,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := srv.Start(); err != nil {
				t.Fatalf("start: %v", err)
			}
			defer srv.Stop()

			q := new(dns.Msg)
			q.SetQuestion("ttl.example.", dns.TypeA)
			packed, err := q.Pack()
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.Post(
				"http://"+srv.httpServerDoH.Addr+"/dns-query",
				"application/dns-message",
				bytes.NewReader(packed),
			)
			if err != nil {
				t.Fatalf("doh post: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			if got := resp.Header.Get("Cache-Control"); got != tc.want {
				t.Fatalf("Cache-Control=%q, want %q (body: %x)", got, tc.want, body)
			}
		})
	}
}

// A synthetic empty answer (IPv6 disabled -> empty AAAA) carries no RRs and
// must tell HTTP caches not to store it.
func TestDoHCacheControlNoStoreForEmptyAnswer(t *testing.T) {
	stub := localDoHTTPServer(t, nil)
	srv, err := NewServer(Config{
		ListenAddr:    "127.0.0.1:0",
		DoHListenAddr: reserveFreeTCPPort(t),
		DoHURL:        stub.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	q := new(dns.Msg)
	q.SetQuestion("empty.example.", dns.TypeAAAA)
	packed, _ := q.Pack()
	resp, err := http.Post(
		"http://"+srv.httpServerDoH.Addr+"/dns-query",
		"application/dns-message",
		bytes.NewReader(packed),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q, want no-store", got)
	}
}

// doHCacheControl must take the minimum over every section and ignore the
// TTL-less OPT (EDNS0) pseudo-record.
func TestDoHCacheControlDirective(t *testing.T) {
	base := func() *dns.Msg {
		m := new(dns.Msg)
		m.SetReply(&dns.Msg{})
		return m
	}

	m := base()
	if got := doHCacheControl(m); got != "no-store" {
		t.Fatalf("empty msg: %q, want no-store", got)
	}

	m = base()
	m.Answer = []dns.RR{&dns.A{
		Hdr: dns.RR_Header{Name: "a.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 300},
		A:   net.ParseIP("1.2.3.4"),
	}}
	m.Extra = append(m.Extra, &dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT}})
	if got := doHCacheControl(m); got != "max-age=300" {
		t.Fatalf("OPT must not influence TTL: %q", got)
	}

	m.Ns = []dns.RR{&dns.SOA{
		Hdr:     dns.RR_Header{Name: "example.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 5},
		Ns:      "ns.example.",
		Mbox:    "hostmaster.example.",
		Serial:  1,
		Refresh: 3600,
		Retry:   900,
		Expire:  86400,
		Minttl:  60,
	}}
	if got := doHCacheControl(m); got != "max-age=5" {
		t.Fatalf("min over sections: %q, want max-age=5", got)
	}
}

// The DoH HTTP server must bound header reading and idle keep-alive time so a
// network-facing listener cannot be pinned by Slowloris-style connections.
func TestDoHServerHasReadDeadlines(t *testing.T) {
	srv, err := NewServer(Config{
		ListenAddr:    "127.0.0.1:0",
		DoHListenAddr: reserveFreeTCPPort(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	if srv.httpServerDoH.ReadHeaderTimeout <= 0 {
		t.Fatal("ReadHeaderTimeout not set: slow request headers hold connections forever")
	}
	if srv.httpServerDoH.IdleTimeout <= 0 {
		t.Fatal("IdleTimeout not set: idle keep-alive connections are never reclaimed")
	}
}

// RFC 1034 3.6 / RFC 2181 8: a caching resolver must age RR TTLs before
// serving a cached answer. The old code returned the original upstream TTL
// on every cache hit, so a record seconds from expiry here was handed out as
// freshly valid, and the DoH Cache-Control header was inflated too.
func TestCacheHitAgesRRsTTLs(t *testing.T) {
	srv, err := NewServer(Config{ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	const name = "aged.example."
	question := dns.Question{Name: name, Qtype: dns.TypeA, Qclass: dns.ClassINET}

	cached := new(dns.Msg)
	cached.SetReply(&dns.Msg{Question: []dns.Question{question}})
	cached.Answer = append(cached.Answer,
		&dns.A{
			Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 100},
			A:   net.ParseIP("203.0.113.5"),
		},
		&dns.A{
			Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 130},
			A:   net.ParseIP("203.0.113.6"),
		},
	)
	cached.Ns = append(cached.Ns, &dns.SOA{
		Hdr:     dns.RR_Header{Name: "example.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 200},
		Ns:      "ns.example.",
		Mbox:    "hostmaster.example.",
		Serial:  1,
		Refresh: 3600,
		Retry:   900,
		Expire:  86400,
		Minttl:  60,
	})

	// The entry has lived in cache for 90s and has 10s left until expiry.
	now := time.Now()
	srv.cache.Store("aged.example_1_1", DNSCacheEntry{
		Msg:       cached,
		StoredAt:  now.Add(-90 * time.Second),
		ExpiresAt: now.Add(10 * time.Second),
	})

	req := &dns.Msg{Question: []dns.Question{question}}
	resp, err := srv.ResolveMsg(req)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(resp.Answer) != 2 {
		t.Fatalf("answers=%d", len(resp.Answer))
	}
	want := []uint32{10, 40}
	for i, rr := range resp.Answer {
		if got := rr.Header().Ttl; got != want[i] {
			t.Fatalf("answer %d TTL=%d, want %d (100/130 minus 90s)", i, got, want[i])
		}
	}
	if got := resp.Ns[0].Header().Ttl; got != 110 {
		t.Fatalf("authority TTL=%d, want 110 (200 minus 90s)", got)
	}
	// The Cache-Control directive is derived from the aged TTLs.
	if got := doHCacheControl(resp); got != "max-age=10" {
		t.Fatalf("Cache-Control=%q, want max-age=10", got)
	}
}

// A GET dns= parameter decoding to more than one DNS message must be rejected
// just like an oversized POST; the GET path is only bounded by header size.
func TestDoHHTTPRejectsOversizedGET(t *testing.T) {
	srv, err := NewServer(Config{
		ListenAddr:    "127.0.0.1:0",
		DoHListenAddr: reserveFreeTCPPort(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	blob := bytes.Repeat([]byte{0x41}, maxDNSMessageSize+1)
	u := "http://" + srv.httpServerDoH.Addr + "/dns-query?dns=" +
		base64.RawURLEncoding.EncodeToString(blob)
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("oversized get: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized GET status=%d, want 400", resp.StatusCode)
	}
}

// startTruncatingUDPDNS serves the same address over UDP and TCP: UDP always
// returns a truncated (TC) empty answer, TCP returns the full answer.
func startTruncatingUDPDNS(t *testing.T, fullAnswer *dns.Msg) string {
	t.Helper()
	addr := reserveFreeTCPPort(t)

	h := dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		resp := new(dns.Msg)
		resp.SetReply(m)
		if w.RemoteAddr().Network() == "udp" {
			resp.Truncated = true
		} else {
			resp.Answer = append(resp.Answer, fullAnswer.Answer...)
		}
		_ = w.WriteMsg(resp)
	})

	udpConn, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Fatalf("udp listen: %v", err)
	}
	tcpLn, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("tcp listen: %v", err)
	}

	udpSrv := &dns.Server{PacketConn: udpConn.(*net.UDPConn), Net: "udp", Handler: h}
	tcpSrv := &dns.Server{Listener: tcpLn, Net: "tcp", Handler: h}
	go func() { _ = udpSrv.ActivateAndServe() }()
	go func() { _ = tcpSrv.ActivateAndServe() }()
	t.Cleanup(func() {
		_ = udpSrv.Shutdown()
		_ = tcpSrv.Shutdown()
	})
	return addr
}

// RFC 2181 9 / RFC 7766: when the direct upstream sets TC over UDP the
// resolver must retry the question over TCP instead of handing out the
// truncated answer (which is outright invalid over an inbound TCP session).
func TestResolveDirectRetriesTCPOnTruncation(t *testing.T) {
	full := new(dns.Msg)
	full.Answer = append(full.Answer, &dns.A{
		Hdr: dns.RR_Header{Name: "big.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
		A:   net.ParseIP("198.51.100.20"),
	})
	upstream := startTruncatingUDPDNS(t, full)

	srv, err := NewServer(Config{ListenAddr: "127.0.0.1:0", DirectDNS: upstream})
	if err != nil {
		t.Fatal(err)
	}
	req := new(dns.Msg)
	req.SetQuestion("big.example.", dns.TypeA)

	resp, err := srv.resolveDirect(req, true)
	if err != nil {
		t.Fatalf("resolveDirect: %v", err)
	}
	if resp.Truncated {
		t.Fatal("truncated UDP answer returned without TCP retry")
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("answers=%d, want the full TCP answer", len(resp.Answer))
	}
	if a, ok := resp.Answer[0].(*dns.A); !ok || !a.A.Equal(net.ParseIP("198.51.100.20")) {
		t.Fatalf("unexpected answer: %v", resp.Answer)
	}
}

// If the upstream truncates over UDP and its TCP listener is unavailable:
//   - over an inbound UDP session the truncated response is still returned
//     (with an error logged) so the client may retry over TCP itself;
//   - over an inbound TCP/DoH session a truncated reply is invalid
//     (RFC 7766), so the lookup must fail instead of carrying the TC bit.
func TestResolveDirectTruncatedTCPFallbackFailure(t *testing.T) {
	addr := reserveFreeTCPPort(t)
	udpConn, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	h := dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		resp := new(dns.Msg)
		resp.SetReply(m)
		resp.Truncated = true
		_ = w.WriteMsg(resp)
	})
	udpSrv := &dns.Server{PacketConn: udpConn.(*net.UDPConn), Net: "udp", Handler: h}
	go func() { _ = udpSrv.ActivateAndServe() }()
	t.Cleanup(func() { _ = udpSrv.Shutdown() })
	// No TCP listener on addr: the fallback dial is refused immediately.

	srv, err := NewServer(Config{ListenAddr: "127.0.0.1:0", DirectDNS: addr})
	if err != nil {
		t.Fatal(err)
	}
	req := new(dns.Msg)
	req.SetQuestion("big.example.", dns.TypeA)

	resp, err := srv.resolveDirect(req, false)
	if err != nil {
		t.Fatalf("UDP inbound: truncated answer must be returned, got err=%v", err)
	}
	if resp == nil || !resp.Truncated {
		t.Fatalf("UDP inbound: expected truncated fallback response, got %v", resp)
	}

	if resp, err := srv.resolveDirect(req, true); err == nil {
		t.Fatalf("TCP inbound: expected error, got truncated=%v", resp != nil && resp.Truncated)
	}
}

// End-to-end: with an upstream that only serves TC over UDP and has no TCP
// listener, an inbound UDP client still gets TC (a valid retry hint), while an
// inbound TCP client gets SERVFAIL instead of an invalid TC reply. The
// truncated answer must never enter the cache.
func TestDNSInboundTransportAwareTruncation(t *testing.T) {
	upstream := reserveFreeTCPPort(t)
	udpConn, err := net.ListenPacket("udp", upstream)
	if err != nil {
		t.Fatal(err)
	}
	h := dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		resp := new(dns.Msg)
		resp.SetReply(m)
		resp.Truncated = true
		_ = w.WriteMsg(resp)
	})
	udpSrv := &dns.Server{PacketConn: udpConn.(*net.UDPConn), Net: "udp", Handler: h}
	go func() { _ = udpSrv.ActivateAndServe() }()
	t.Cleanup(func() { _ = udpSrv.Shutdown() })

	r := router.NewRouterWithOptions(router.Options{
		DisableBuiltin: true,
		DirectDomains:  []string{"big.example"},
	})
	defer r.Close()

	addr := freeUDPPort(t)
	srv, err := NewServer(Config{ListenAddr: addr, DirectDNS: upstream, Router: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	time.Sleep(100 * time.Millisecond)

	q := new(dns.Msg)
	q.SetQuestion("big.example.", dns.TypeA)

	udpClient := &dns.Client{Net: "udp", Timeout: 2 * time.Second}
	udpResp, _, err := udpClient.Exchange(q, addr)
	if err != nil {
		t.Fatalf("udp exchange: %v", err)
	}
	if !udpResp.Truncated {
		t.Fatal("UDP client must receive the TC=1 retry hint")
	}

	tcpClient := &dns.Client{Net: "tcp", Timeout: 2 * time.Second}
	tcpResp, _, err := tcpClient.Exchange(q, addr)
	if err != nil {
		t.Fatalf("tcp exchange: %v", err)
	}
	if tcpResp.Truncated {
		t.Fatal("TC bit must never be set on a reply delivered over TCP")
	}
	if tcpResp.Rcode != dns.RcodeServerFailure {
		t.Fatalf("TCP client rcode=%s, want SERVFAIL", dns.RcodeToString[tcpResp.Rcode])
	}

	if _, cached := srv.cache.Load("big.example_1_1"); cached {
		t.Fatal("truncated answer must not be cached")
	}
}
