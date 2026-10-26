package dns

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/yellowsocks/core/stats"
	"github.com/miekg/dns"
)

// TestMain speeds the janitor up once, before any goroutine exists, so the
// sweep loop runs (and is covered) during tests instead of every 2 minutes.
// Writing the package variable here cannot race with a janitor goroutine:
// none exist yet, and nothing writes it afterwards.
func TestMain(m *testing.M) {
	cacheSweepInterval = 20 * time.Millisecond
	os.Exit(m.Run())
}

// TestSetupDoHClientDefault: without a socks5 upstream the DoH client is a
// plain transport with the 5s timeout.
func TestSetupDoHClientDefault(t *testing.T) {
	s, err := NewServer(Config{ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if s.httpClientDoH == nil {
		t.Fatal("DoH client missing")
	}
	if s.httpClientDoH.Timeout != 5*time.Second {
		t.Fatalf("DoH client timeout = %v, want 5s", s.httpClientDoH.Timeout)
	}
	tr, ok := s.httpClientDoH.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T", s.httpClientDoH.Transport)
	}
	if tr.DialContext != nil {
		t.Fatal("no socks5 configured: the transport must use the default dialer")
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("DoH transport must verify upstream TLS certificates")
	}
}

// TestStartFailureUDPBind: an unusable listen address fails synchronously.
func TestStartFailureUDPBind(t *testing.T) {
	s, err := NewServer(Config{ListenAddr: "nosuch-host.invalid:53"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err == nil {
		t.Fatal("expected Start to fail on an unresolvable UDP address")
	}
	_ = s.Stop()
}

// TestStartFailureTCPBind: UDP binds, the TCP listener on the same port
// cannot, and Start reports the failure after releasing the UDP socket.
func TestStartFailureTCPBind(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	addr := occupied.Addr().String()

	s, err := NewServer(Config{ListenAddr: addr})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err == nil {
		t.Fatal("expected Start to fail when the TCP DNS port is occupied")
	}
	_ = s.Stop()

	// The UDP socket bound before the failure must be released.
	deadline := time.Now().Add(2 * time.Second)
	for {
		pc, lerr := net.ListenPacket("udp", addr)
		if lerr == nil {
			_ = pc.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("UDP port %s still bound after failed Start: %v", addr, lerr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestStartDoTCertLoadFailure: a DoT listener with an unusable certificate
// fails Start cleanly.
func TestStartDoTCertLoadFailure(t *testing.T) {
	dir := t.TempDir()
	s, err := NewServer(Config{
		ListenAddr:    "127.0.0.1:0",
		DoTListenAddr: reserveFreeTCPPort(t),
		TLSCertFile:   dir + "/missing.pem",
		TLSKeyFile:    dir + "/missing-key.pem",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err == nil {
		t.Fatal("expected Start to fail when the DoT certificate is missing")
	}
	_ = s.Stop()
}

// TestStartDoTBindFailure: a valid certificate but an occupied DoT port fails
// Start after the earlier listeners are released.
func TestStartDoTBindFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	certFile, keyFile := writeTestTLSCert(t, t.TempDir())
	s, err := NewServer(Config{
		ListenAddr:    "127.0.0.1:0",
		DoTListenAddr: occupied.Addr().String(),
		TLSCertFile:   certFile,
		TLSKeyFile:    keyFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err == nil {
		t.Fatal("expected Start to fail when the DoT port is occupied")
	}
	_ = s.Stop()
}

// TestStartAndStopWithDoT: a fully started DoT server shuts down without
// error and releases its port.
func TestStartAndStopWithDoT(t *testing.T) {
	dohStub := localDoHTTPServer(t, map[string]string{"dot.example": "203.0.113.9"})
	certFile, keyFile := writeTestTLSCert(t, t.TempDir())
	dotAddr := reserveFreeTCPPort(t)

	s, err := NewServer(Config{
		ListenAddr:    freeUDPPort(t),
		DoTListenAddr: dotAddr,
		TLSCertFile:   certFile,
		TLSKeyFile:    keyFile,
		DoHURL:        dohStub.URL + "/dns-query",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := s.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// The DoT port must be free again after Stop.
	deadline := time.Now().Add(2 * time.Second)
	for {
		ln, lerr := net.Listen("tcp", dotAddr)
		if lerr == nil {
			_ = ln.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("DoT port %s still bound after Stop: %v", dotAddr, lerr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestStopIdempotent: a second Stop is a no-op and returns no error.
func TestStopIdempotent(t *testing.T) {
	s, err := NewServer(Config{ListenAddr: freeUDPPort(t), DoHListenAddr: reserveFreeTCPPort(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("first stop: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}

// TestJanitorSweepsOnTick: the background janitor reaps expired state without
// a test calling sweepExpired by hand. TestMain has shrunk the sweep interval
// so the ticker branch fires quickly.
func TestJanitorSweepsOnTick(t *testing.T) {
	s, err := NewServer(Config{ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	s.cache.Store("stale.example_1_1", DNSCacheEntry{Msg: new(dns.Msg), ExpiresAt: time.Now().Add(-time.Minute)})
	s.ipToDomain.Store("9.9.9.9", ipMapping{domain: "stale.example", expires: time.Now().Add(-time.Minute)})
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, cacheGone := s.cache.Load("stale.example_1_1")
		_, mapGone := s.ipToDomain.Load("9.9.9.9")
		if !cacheGone && !mapGone {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("janitor did not reap expired state in time (cache=%v map=%v)", cacheGone, mapGone)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDoHHTTPUnpackFailures: wire payloads that decode but are not DNS
// messages are rejected; a resolution failure surfaces as 502.
func TestDoHHTTPUnpackFailures(t *testing.T) {
	s, err := NewServer(Config{
		ListenAddr:    "127.0.0.1:0",
		DoHListenAddr: reserveFreeTCPPort(t),
		DoHURL:        "http://127.0.0.1:1/dns-query", // refused immediately: resolution fails
		DirectDNS:     "127.0.0.1:1",                  // keep the DoH failure fallback offline too
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()
	base := "http://" + s.httpServerDoH.Addr

	// GET with valid base64url of garbage bytes (not a DNS message) -> 400.
	resp, err := http.Get(base + "/dns-query?dns=" + base64.RawURLEncoding.EncodeToString([]byte("\x00garbage")))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET garbage dns message -> %d, want 400", resp.StatusCode)
	}

	// GET with padded base64 (standard URL alphabet) must decode via the
	// padded fallback path.
	q := new(dns.Msg)
	q.SetQuestion("padded.example.", dns.TypeA)
	packed, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.Get(base + "/dns-query?dns=" + base64.URLEncoding.EncodeToString(packed))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("padded GET (dead upstream) -> %d, want 502 (body %x)", resp.StatusCode, body)
	}

	// POST of a valid message to the root path also routes to the DoH
	// handler; the dead upstream turns it into a 502.
	resp, err = http.Post(base+"/", "application/dns-message", bytes.NewReader(packed))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("POST valid message, dead upstream -> %d, want 502", resp.StatusCode)
	}

	// A POST whose body is valid base64-decodable junk but not a DNS message
	// unpacks as garbage -> 400.
	resp, err = http.Post(base+"/dns-query", "application/dns-message", strings.NewReader("###not-a-dns-message###"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST garbage -> %d, want 400", resp.StatusCode)
	}
}

// TestQtypeString: known types render by name, unknown ones as TYPE<N>.
func TestQtypeString(t *testing.T) {
	cases := map[uint16]string{
		dns.TypeA:     "A",
		dns.TypeAAAA:  "AAAA",
		dns.TypeCNAME: "CNAME",
		dns.TypeHTTPS: "HTTPS",
		9999:          "TYPE9999",
		0:             "None", // miekg's name for the pseudo type 0
	}
	for qtype, want := range cases {
		if got := qtypeString(qtype); got != want {
			t.Fatalf("qtypeString(%d) = %q, want %q", qtype, got, want)
		}
	}
}

// TestAgeRRsTTLFloor: TTLs at or below the elapsed time floor at zero, larger
// ones are decremented. OPT pseudo-records are left alone.
func TestAgeRRsTTLFloor(t *testing.T) {
	m := new(dns.Msg)
	m.SetReply(&dns.Msg{})
	m.Answer = append(m.Answer,
		&dns.A{Hdr: dns.RR_Header{Name: "a.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 150}, A: net.ParseIP("1.2.3.4")},
		&dns.A{Hdr: dns.RR_Header{Name: "a.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 100}, A: net.ParseIP("1.2.3.5")},
		&dns.A{Hdr: dns.RR_Header{Name: "a.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 40}, A: net.ParseIP("1.2.3.6")},
	)
	m.Extra = append(m.Extra, &dns.OPT{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeOPT, Ttl: 12345}})

	ageRRsTTL(m, 100)

	want := []uint32{50, 0, 0}
	for i, rr := range m.Answer {
		if got := rr.Header().Ttl; got != want[i] {
			t.Fatalf("answer %d TTL = %d, want %d", i, got, want[i])
		}
	}
	// The OPT record is a hop-by-hop pseudo-RR: its header is not aged.
	if got := m.Extra[0].Header().Ttl; got != 12345 {
		t.Fatalf("OPT TTL = %d, want it untouched", got)
	}
}

// TestResponseMatchesQueryTable walks every rejection branch of the question
// sanity check plus the case-insensitive acceptance path.
func TestResponseMatchesQueryTable(t *testing.T) {
	req := new(dns.Msg)
	req.SetQuestion("Good.Example.", dns.TypeA)

	okReply := func() *dns.Msg {
		resp := new(dns.Msg)
		resp.SetReply(req)
		return resp
	}

	if responseMatchesQuery(req, nil) {
		t.Fatal("nil response must not match")
	}
	if responseMatchesQuery(req, new(dns.Msg)) {
		t.Fatal("a query (QR=0) is not an answer")
	}
	wrongOpcode := okReply()
	wrongOpcode.Opcode = dns.OpcodeStatus
	if responseMatchesQuery(req, wrongOpcode) {
		t.Fatal("opcode mismatch must be rejected")
	}
	noQuestion := okReply()
	noQuestion.Question = nil
	if responseMatchesQuery(req, noQuestion) {
		t.Fatal("a different question count must be rejected")
	}
	wrongType := okReply()
	wrongType.Question[0].Qtype = dns.TypeAAAA
	if responseMatchesQuery(req, wrongType) {
		t.Fatal("qtype mismatch must be rejected")
	}
	wrongClass := okReply()
	wrongClass.Question[0].Qclass = dns.ClassCHAOS
	if responseMatchesQuery(req, wrongClass) {
		t.Fatal("qclass mismatch must be rejected")
	}
	wrongName := okReply()
	wrongName.Question[0].Name = "evil.example."
	if responseMatchesQuery(req, wrongName) {
		t.Fatal("name mismatch must be rejected")
	}
	// Case differences in the name are tolerated.
	caseVariant := okReply()
	caseVariant.Question[0].Name = "GOOD.example."
	if !responseMatchesQuery(req, caseVariant) {
		t.Fatal("case-only name difference must match")
	}
	// Multi-question messages compare element-wise.
	twoReq := req.Copy()
	twoReq.Question = append(twoReq.Question, dns.Question{Name: "second.example.", Qtype: dns.TypeAAAA, Qclass: dns.ClassINET})
	twoResp := twoReq.Copy()
	twoResp.Response = true
	if !responseMatchesQuery(twoReq, twoResp) {
		t.Fatal("identical two-question exchange must match")
	}
	twoResp.Question[1].Qtype = dns.TypeA
	if responseMatchesQuery(twoReq, twoResp) {
		t.Fatal("second question mismatch must be rejected")
	}
}

// TestResolveEmptyQuestion: a message without questions is an error, not a
// panic.
func TestResolveEmptyQuestion(t *testing.T) {
	s, err := NewServer(Config{ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := s.ResolveMsg(new(dns.Msg)); err == nil || resp != nil {
		t.Fatalf("empty question resolved to %v, want an error", resp)
	}
}

// TestResolveExpiredCacheEntry: a stale cache entry is dropped and the
// question goes upstream again.
func TestResolveExpiredCacheEntry(t *testing.T) {
	stub, hits := dohAnsweringCounting(t, func(req, resp *dns.Msg) {})
	s, err := NewServer(Config{ListenAddr: "127.0.0.1:0", DoHURL: stub.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	q := new(dns.Msg)
	q.SetQuestion("stale.example.", dns.TypeA)
	if _, err := s.ResolveMsg(q); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("upstream hits = %d, want 1", n)
	}

	// Force the entry to look expired but keep its stored timestamp so the
	// TTL aging on the next hit has a baseline.
	key := "stale.example_1_1"
	entry, ok := s.cache.Load(key)
	if !ok {
		t.Fatal("first answer must be cached")
	}
	e := entry.(DNSCacheEntry)
	e.ExpiresAt = time.Now().Add(-time.Second)
	s.cache.Store(key, e)

	if _, err := s.ResolveMsg(q); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if n := hits.Load(); n != 2 {
		t.Fatalf("upstream hits = %d, want 2: the expired entry must be re-queried", n)
	}
	fresh, ok := s.cache.Load(key)
	if !ok {
		t.Fatal("re-resolved answer must be cached again")
	}
	if time.Until(fresh.(DNSCacheEntry).ExpiresAt) <= 0 {
		t.Fatal("re-cached entry must carry a fresh expiry")
	}
}

// TestResolveDoHUpstreamErrors: transport errors, non-200 answers and
// garbage bodies all fail the DoH path (and the caller falls back or fails).
func TestResolveDoHUpstreamErrors(t *testing.T) {
	t.Run("unparseable upstream url", func(t *testing.T) {
		s, err := NewServer(Config{ListenAddr: "127.0.0.1:0", DoHURL: "http://bad url/dns-query"})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Stop()
		q := new(dns.Msg)
		q.SetQuestion("url.example.", dns.TypeA)
		if _, err := s.resolveDoH(q); err == nil {
			t.Fatal("expected an error for an unparseable DoH URL")
		}
	})

	t.Run("http 500", func(t *testing.T) {
		stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer stub.Close()
		s, err := NewServer(Config{ListenAddr: "127.0.0.1:0", DoHURL: stub.URL})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Stop()
		q := new(dns.Msg)
		q.SetQuestion("status.example.", dns.TypeA)
		if _, err := s.resolveDoH(q); err == nil || !strings.Contains(fmt.Sprint(err), "doh status error") {
			t.Fatalf("expected a doh status error, got %v", err)
		}
	})

	t.Run("garbage body", func(t *testing.T) {
		stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/dns-message")
			_, _ = w.Write([]byte("this is not a dns message"))
		}))
		defer stub.Close()
		s, err := NewServer(Config{ListenAddr: "127.0.0.1:0", DoHURL: stub.URL})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Stop()
		q := new(dns.Msg)
		q.SetQuestion("unpack.example.", dns.TypeA)
		if _, err := s.resolveDoH(q); err == nil {
			t.Fatal("expected an unpack error for a garbage upstream body")
		}
	})
}

// TestResolveDoHFallsBackToDirect: a failing DoH upstream falls back to the
// direct resolver, which serves the answer.
func TestResolveDoHFallsBackToDirect(t *testing.T) {
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
			if req.Unpack(buf[:n]) != nil || len(req.Question) == 0 {
				continue
			}
			resp := new(dns.Msg)
			resp.SetReply(req)
			resp.Answer = append(resp.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.ParseIP("7.7.7.7"),
			})
			if packed, perr := resp.Pack(); perr == nil {
				_, _ = pc.WriteTo(packed, addr)
			}
		}
	}()

	s, err := NewServer(Config{
		ListenAddr: "127.0.0.1:0",
		DoHURL:     "http://127.0.0.1:1/dns-query", // dead: forces the fallback
		DirectDNS:  pc.LocalAddr().String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	q := new(dns.Msg)
	q.SetQuestion("fallback.example.", dns.TypeA)
	resp, err := s.ResolveMsg(q)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("answers = %d, want the direct upstream's", len(resp.Answer))
	}
	if a, ok := resp.Answer[0].(*dns.A); !ok || !a.A.Equal(net.ParseIP("7.7.7.7")) {
		t.Fatalf("unexpected answer: %v", resp.Answer)
	}
}

// TestResolveDirectTCPRetryMismatchedQuestion: when the upstream truncates
// over UDP and its TCP answer is for a different question, the retry must be
// rejected rather than served.
func TestResolveDirectTCPRetryMismatchedQuestion(t *testing.T) {
	addr := reserveFreeTCPPort(t)
	udpConn, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	tcpLn, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	h := dns.HandlerFunc(func(w dns.ResponseWriter, m *dns.Msg) {
		resp := new(dns.Msg)
		resp.SetReply(m)
		if w.RemoteAddr().Network() == "udp" {
			resp.Truncated = true
		} else {
			// The TCP retry answers a different question.
			resp.Question = []dns.Question{{Name: "evil.example.", Qtype: dns.TypeA, Qclass: dns.ClassINET}}
			resp.Answer = append(resp.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: "evil.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.ParseIP("6.6.6.6"),
			})
		}
		_ = w.WriteMsg(resp)
	})
	udpSrv := &dns.Server{PacketConn: udpConn.(*net.UDPConn), Net: "udp", Handler: h}
	tcpSrv := &dns.Server{Listener: tcpLn, Net: "tcp", Handler: h}
	go func() { _ = udpSrv.ActivateAndServe() }()
	go func() { _ = tcpSrv.ActivateAndServe() }()
	t.Cleanup(func() {
		_ = udpSrv.Shutdown()
		_ = tcpSrv.Shutdown()
	})

	s, err := NewServer(Config{ListenAddr: "127.0.0.1:0", DirectDNS: addr})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	q := new(dns.Msg)
	q.SetQuestion("big.example.", dns.TypeA)

	// Over a UDP session the truncated answer is still handed back as a
	// retry hint; the failed TCP retry is only logged.
	resp, err := s.resolveDirect(q, false)
	if err != nil {
		t.Fatalf("UDP session: truncated fallback must be returned, got %v", err)
	}
	if resp == nil || !resp.Truncated {
		t.Fatalf("UDP session: expected a truncated fallback, got %+v", resp)
	}

	// Over an inbound TCP/DoH session the failed (mismatched) retry must
	// fail the lookup instead of forwarding the TC bit.
	if resp, err := s.resolveDirect(q, true); err == nil {
		t.Fatalf("TCP session: mismatched TCP retry must be rejected, got truncated=%v", resp != nil && resp.Truncated)
	}
}

// TestLookupDomainByIPNormalization: IPv4-mapped and differently spelled IPs
// normalize to the stored form, and expired mappings are reclaimed.
func TestLookupDomainByIPNormalization(t *testing.T) {
	s, err := NewServer(Config{ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	s.ipToDomain.Store("1.2.3.4", ipMapping{domain: "v4.example", expires: time.Now().Add(time.Minute)})
	s.ipToDomain.Store("2001:db8::1", ipMapping{domain: "v6.example", expires: time.Now().Add(time.Minute)})
	s.ipToDomain.Store("5.5.5.5", ipMapping{domain: "old.example", expires: time.Now().Add(-time.Minute)})

	if d, ok := s.LookupDomainByIP("::ffff:1.2.3.4"); !ok || d != "v4.example" {
		t.Fatalf("mapped IPv4 lookup = %q, %v", d, ok)
	}
	if d, ok := s.LookupDomainByIP("2001:0db8:0000:0000:0000:0000:0000:0001"); !ok || d != "v6.example" {
		t.Fatalf("expanded IPv6 lookup = %q, %v", d, ok)
	}
	if d, ok := s.LookupDomainByIP("::ffff:7.7.7.7"); ok {
		t.Fatalf("unknown IP returned %q", d)
	}
	if _, ok := s.LookupDomainByIP("5.5.5.5"); ok {
		t.Fatal("expired mapping must be reported as missing")
	}
	if _, ok := s.ipToDomain.Load("5.5.5.5"); ok {
		t.Fatal("expired mapping must be deleted on lookup")
	}
	// Not an IP at all: treated as a literal map key.
	s.ipToDomain.Store("weird-key", ipMapping{domain: "weird.example", expires: time.Now().Add(time.Minute)})
	if d, ok := s.LookupDomainByIP("weird-key"); !ok || d != "weird.example" {
		t.Fatalf("literal key lookup = %q, %v", d, ok)
	}
}

// TestResolveRecordsStats: ResolveMsg accounting lands in the collector for
// both the DoH and synthetic paths.
func TestResolveRecordsStats(t *testing.T) {
	stub := localDoHTTPServer(t, map[string]string{"counted.example": "4.4.4.4"})
	c := stats.New(stats.Config{BucketInterval: time.Minute})
	s, err := NewServer(Config{ListenAddr: "127.0.0.1:0", DoHURL: stub.URL + "/dns-query", Stats: c})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	q := new(dns.Msg)
	q.SetQuestion("counted.example.", dns.TypeA)
	if _, err := s.ResolveMsg(q); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := c.DNSStats(0); got.Queries != 1 || got.DoH != 1 {
		t.Fatalf("dns stats = %+v, want one DoH query", got)
	}

	// The cache hit path records upstream=cache.
	if _, err := s.ResolveMsg(q); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if got := c.DNSStats(0); got.CacheHits != 1 {
		t.Fatalf("cache hits = %d, want 1", got.CacheHits)
	}
}

// TestResolveAAAAFailureStillAnswered: even when IPv6 is enabled and the
// upstream fails, an AAAA query gets an empty NOERROR instead of an error.
func TestResolveAAAAFailureStillAnswered(t *testing.T) {
	s, err := NewServer(Config{
		ListenAddr: "127.0.0.1:0",
		DoHURL:     "http://127.0.0.1:1/dns-query", // dead upstream
		EnableIPv6: true,
		DirectDNS:  "127.0.0.1:1", // dead direct upstream too
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	q := new(dns.Msg)
	q.SetQuestion("v6fail.example.", dns.TypeAAAA)
	resp, err := s.ResolveMsg(q)
	if err != nil {
		t.Fatalf("AAAA failure must degrade to an empty answer: %v", err)
	}
	if len(resp.Answer) != 0 || resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected an empty NOERROR AAAA, got %v (rcode %d)", resp.Answer, resp.Rcode)
	}
}
