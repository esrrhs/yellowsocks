package dns

import (
	"bytes"
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
					Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
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
