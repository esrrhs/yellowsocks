package proxy

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/gohome/network"
	appdns "github.com/esrrhs/yellowsocks/core/dns"
	"github.com/esrrhs/yellowsocks/core/router"
	mdns "github.com/miekg/dns"
)

// proxyRouterWithLocalhost forces the loopback-reachable name "localhost" to
// take the Proxy (SPP) decision. The mock upstream resolves localhost itself,
// so proxy-route traffic can still reach a loopback test target end to end.
func proxyRouterWithLocalhost(t *testing.T) *router.Router {
	t.Helper()
	r := router.NewRouter()
	t.Cleanup(r.Close)
	r.AddProxyDomain("localhost")
	if d := r.Decide("localhost", nil); d != router.Proxy {
		t.Fatalf("test setup: localhost decided %v, want Proxy", d)
	}
	return r
}

// A SOCKS5 CONNECT to a proxy-route target must traverse the upstream SPP
// SOCKS5, not be dialed directly.
func TestSocks5ConnectProxiedThroughUpstream(t *testing.T) {
	echoAddr, stopEcho := startEchoTCP(t)
	defer stopEcho()
	upstreamAddr, stopUpstream := startMockUpstreamSocks5(t)
	defer stopUpstream()

	s5 := NewSocks5Server(Socks5Config{
		ListenAddr: "127.0.0.1:0",
		Router:     proxyRouterWithLocalhost(t),
		Upstream:   &mockUpstream{addr: upstreamAddr},
	})
	if err := s5.Start(); err != nil {
		t.Fatal(err)
	}
	defer s5.Stop()

	cc, err := net.DialTCP("tcp", nil, mustTCPAddr(t, s5.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if err := network.Sock5Handshake(cc, 5000, "", ""); err != nil {
		t.Fatal(err)
	}
	_, portStr, _ := net.SplitHostPort(echoAddr)
	port, _ := strconv.Atoi(portStr)
	// Domain ATYP: the name decides the route; the upstream resolves it.
	if err := network.Sock5SetRequest(cc, "localhost", port, 5000); err != nil {
		t.Fatalf("connect through upstream: %v", err)
	}
	msg := []byte("over the spp tunnel")
	if _, err := cc.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(cc, got); err != nil {
		t.Fatalf("echo through upstream: %v", err)
	}
	if string(got) != string(msg) {
		t.Fatalf("got %q", got)
	}
}

// An HTTP CONNECT to a proxy-route target must tunnel through the upstream.
func TestHTTPConnectProxiedThroughUpstream(t *testing.T) {
	echoAddr, stopEcho := startEchoTCP(t)
	defer stopEcho()
	upstreamAddr, stopUpstream := startMockUpstreamSocks5(t)
	defer stopUpstream()

	hp := NewHTTPServer(HTTPConfig{
		ListenAddr: "127.0.0.1:0",
		Router:     proxyRouterWithLocalhost(t),
		Upstream:   &mockUpstream{addr: upstreamAddr},
	})
	if err := hp.Start(); err != nil {
		t.Fatal(err)
	}
	defer hp.Stop()

	cc, err := net.Dial("tcp", hp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	// Hostname (not an IP literal) so the router takes the Proxy branch; the
	// upstream resolves localhost to the loopback echo itself.
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n",
		net.JoinHostPort("localhost", portOf(t, echoAddr)),
		net.JoinHostPort("localhost", portOf(t, echoAddr)))
	if _, err := cc.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(cc)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte(line), []byte("200 Connection Established")) {
		t.Fatalf("want 200, got %s", line)
	}
	// Drain blank line.
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	msg := []byte("connect tunnel bytes")
	if _, err := cc.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(cc, got); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if string(got) != string(msg) {
		t.Fatalf("got %q", got)
	}
}

// A plain (non-CONNECT) HTTP request to a proxy-route target must be forwarded
// through the upstream with an origin-form request line.
func TestHTTPGetProxiedThroughUpstream(t *testing.T) {
	bl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer bl.Close()
	target := net.JoinHostPort("localhost", portOf(t, bl.Addr().String()))

	backend := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "" || r.RequestURI[0] != '/' {
			http.Error(w, "expected origin-form URI", http.StatusBadRequest)
			return
		}
		if r.Host != target {
			http.Error(w, "Host header not forwarded: "+r.Host, http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("upstream-body"))
	})}
	go func() { _ = backend.Serve(bl) }()

	upstreamAddr, stopUpstream := startMockUpstreamSocks5(t)
	defer stopUpstream()

	hp := NewHTTPServer(HTTPConfig{
		ListenAddr: "127.0.0.1:0",
		Router:     proxyRouterWithLocalhost(t),
		Upstream:   &mockUpstream{addr: upstreamAddr},
	})
	if err := hp.Start(); err != nil {
		t.Fatal(err)
	}
	defer hp.Stop()

	cc, err := net.Dial("tcp", hp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	req := fmt.Sprintf("GET http://%s/path HTTP/1.1\r\nHost: %s\r\nProxy-Connection: keep-alive\r\n\r\n",
		target, target)
	if _, err := cc.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(cc), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "upstream-body" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}

// When the upstream SPP is unreachable a proxied SOCKS5 CONNECT must complete
// with a failure reply instead of hanging.
func TestSocks5ProxyRouteFailsWhenUpstreamDown(t *testing.T) {
	s5 := NewSocks5Server(Socks5Config{
		ListenAddr: "127.0.0.1:0",
		Router:     proxyRouterWithLocalhost(t),
		Upstream:   &mockUpstream{addr: "127.0.0.1:1"},
	})
	if err := s5.Start(); err != nil {
		t.Fatal(err)
	}
	defer s5.Stop()

	cc, err := net.DialTCP("tcp", nil, mustTCPAddr(t, s5.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if err := network.Sock5Handshake(cc, 5000, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := network.Sock5SetRequest(cc, "localhost", 443, 5000); err == nil {
		t.Fatal("expected SOCKS5 failure reply with upstream down")
	}
}

func TestHTTPProxyRouteFailsWhenUpstreamDown(t *testing.T) {
	hp := NewHTTPServer(HTTPConfig{
		ListenAddr: "127.0.0.1:0",
		Router:     proxyRouterWithLocalhost(t),
		Upstream:   &mockUpstream{addr: "127.0.0.1:1"},
	})
	if err := hp.Start(); err != nil {
		t.Fatal(err)
	}
	defer hp.Stop()

	for _, req := range []string{
		"CONNECT localhost:443 HTTP/1.1\r\nHost: localhost:443\r\n\r\n",
		"GET http://localhost:443/ HTTP/1.1\r\nHost: localhost:443\r\n\r\n",
	} {
		cc, err := net.Dial("tcp", hp.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cc.Write([]byte(req)); err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(cc)
		line, err := br.ReadString('\n')
		_ = cc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains([]byte(line), []byte("502 Bad Gateway")) {
			t.Fatalf("want 502 for %q, got %s", req, line)
		}
	}
}

// An unsupported address type must produce an X'08' (address type not
// supported) reply, and an unknown command an X'07' (command not supported).
func TestSocks5RequestErrorReplies(t *testing.T) {
	s5 := NewSocks5Server(Socks5Config{
		ListenAddr: "127.0.0.1:0",
		Router:     router.NewRouter(),
	})
	defer func() { _ = s5.Stop() }()
	if err := s5.Start(); err != nil {
		t.Fatal(err)
	}

	readReply := func(t *testing.T, cc net.Conn) byte {
		t.Helper()
		_ = cc.SetReadDeadline(time.Now().Add(2 * time.Second))
		var reply [10]byte
		if _, err := io.ReadFull(cc, reply[:]); err != nil {
			t.Fatalf("read reply: %v", err)
		}
		if reply[0] != 0x05 {
			t.Fatalf("reply ver=%d", reply[0])
		}
		return reply[1]
	}

	// CONNECT with reserved/invalid ATYP 0x02 -> 0x08.
	cc, err := net.Dial("tcp", s5.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := network.Sock5Handshake(cc.(*net.TCPConn), 5000, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := cc.Write([]byte{0x05, 0x01, 0x00, 0x02}); err != nil {
		t.Fatal(err)
	}
	if rep := readReply(t, cc); rep != 0x08 {
		t.Fatalf("invalid atyp reply=%02x, want 08", rep)
	}
	_ = cc.Close()

	// Unknown command 0x09 with a valid IPv4 target -> 0x07.
	cc, err = net.Dial("tcp", s5.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if err := network.Sock5Handshake(cc.(*net.TCPConn), 5000, "", ""); err != nil {
		t.Fatal(err)
	}
	req := []byte{0x05, 0x09, 0x00, 0x01, 127, 0, 0, 1, 0x00, 0x50}
	if _, err := cc.Write(req); err != nil {
		t.Fatal(err)
	}
	if rep := readReply(t, cc); rep != 0x07 {
		t.Fatalf("unknown cmd reply=%02x, want 07", rep)
	}
}

// A malformed SOCKS5 method negotiation must be rejected and the connection
// closed rather than served.
func TestSocks5HandshakeGarbageCloses(t *testing.T) {
	s5 := NewSocks5Server(Socks5Config{
		ListenAddr: "127.0.0.1:0",
		Router:     router.NewRouter(),
	})
	if err := s5.Start(); err != nil {
		t.Fatal(err)
	}
	defer s5.Stop()

	cc, err := net.Dial("tcp", s5.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	_, _ = cc.Write([]byte{0x04, 0x01, 0x00})
	_ = cc.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	if _, err := cc.Read(buf); err == nil {
		t.Fatal("expected connection closed after SOCKS4-style garbage")
	}
}

func portOf(t *testing.T, addr string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %s: %v", addr, err)
	}
	return port
}

// dnsServerWithLearnedIPs starts a DNS server backed by a fake DoH upstream that
// answers the given domain->A map, and primes every ipToDomain entry by resolving the
// names once, the way real traffic does before a client connects to the returned IP.
func dnsServerWithLearnedIPs(t *testing.T, aRecords map[string]string) *appdns.Server {
	t.Helper()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		req := new(mdns.Msg)
		if err := req.Unpack(raw); err != nil || len(req.Question) == 0 {
			http.Error(w, "bad dns message", http.StatusBadRequest)
			return
		}
		resp := new(mdns.Msg)
		resp.SetReply(req)
		q := req.Question[0]
		if q.Qtype == mdns.TypeA {
			name := strings.ToLower(strings.TrimSuffix(q.Name, "."))
			if ipStr, ok := aRecords[name]; ok {
				resp.Answer = append(resp.Answer, &mdns.A{
					Hdr: mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: 60},
					A:   net.ParseIP(ipStr),
				})
			}
		}
		packed, err := resp.Pack()
		if err != nil {
			http.Error(w, "pack", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(packed)
	}))
	t.Cleanup(stub.Close)

	srv, err := appdns.NewServer(appdns.Config{ListenAddr: "127.0.0.1:0", DoHURL: stub.URL})
	if err != nil {
		t.Fatal(err)
	}
	for domain, ip := range aRecords {
		q := new(mdns.Msg)
		q.SetQuestion(domain+".", mdns.TypeA)
		resp, err := srv.ResolveMsg(q)
		if err != nil || len(resp.Answer) == 0 {
			t.Fatalf("prime learned mapping %s -> %s: %v", domain, ip, err)
		}
		if d, ok := srv.LookupDomainByIP(ip); !ok || d != domain {
			t.Fatalf("learned mapping missing for %s: %q %v", ip, d, ok)
		}
	}
	return srv
}

// When the DNS server has learned that 127.0.0.1 belongs to some otherwise
// unknown domain, a client connecting to that IP must still take the reserved-CIDR
// Direct decision. Reparsing the learned domain for the routing IP dropped CIDR/GeoIP
// evaluation entirely, which sent loopback/private traffic up through SPP.
func TestSocks5LearnedDomainKeepsReservedCIDRDecision(t *testing.T) {
	echoAddr, stopEcho := startEchoTCP(t)
	defer stopEcho()

	dnsSrv := dnsServerWithLearnedIPs(t, map[string]string{"leak.test": "127.0.0.1"})
	r := router.NewRouter()
	defer r.Close()

	s5 := NewSocks5Server(Socks5Config{
		ListenAddr: "127.0.0.1:0",
		Router:     r,
		DNS:        dnsSrv,
		// A broken upstream makes a wrong Proxy decision fail loudly instead of
		// silently reaching the echo over some other path.
		Upstream: &mockUpstream{addr: "127.0.0.1:1"},
	})
	if err := s5.Start(); err != nil {
		t.Fatal(err)
	}
	defer s5.Stop()

	cc, err := net.DialTCP("tcp", nil, mustTCPAddr(t, s5.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if err := network.Sock5Handshake(cc, 5000, "", ""); err != nil {
		t.Fatal(err)
	}
	echoPort, _ := strconv.Atoi(portOf(t, echoAddr))
	// Connect to the literal IP: the learned leak.test domain is not on any domain
	// list, so only the 127.0.0.0/8 reserved CIDR can make it Direct.
	if err := network.Sock5SetRequest(cc, "127.0.0.1", echoPort, 5000); err != nil {
		t.Fatalf("expected direct connection via reserved CIDR, got SOCKS failure: %v", err)
	}
	if _, err := cc.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(cc, got); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("got %q", got)
	}
}

// Same guarantee for the HTTP CONNECT path.
func TestHTTPConnectLearnedDomainKeepsReservedCIDRDecision(t *testing.T) {
	echoAddr, stopEcho := startEchoTCP(t)
	defer stopEcho()

	dnsSrv := dnsServerWithLearnedIPs(t, map[string]string{"leak.test": "127.0.0.1"})
	r := router.NewRouter()
	defer r.Close()

	hp := NewHTTPServer(HTTPConfig{
		ListenAddr: "127.0.0.1:0",
		Router:     r,
		DNS:        dnsSrv,
		Upstream:   &mockUpstream{addr: "127.0.0.1:1"},
	})
	if err := hp.Start(); err != nil {
		t.Fatal(err)
	}
	defer hp.Stop()

	cc, err := net.Dial("tcp", hp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if _, err := cc.Write([]byte(fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr))); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(cc)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte(line), []byte("200 Connection Established")) {
		t.Fatalf("expected direct 200 via reserved CIDR, got %s", line)
	}
	// Drain the blank line ending the CONNECT response headers.
	for {
		hdr, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimRight(hdr, "\r\n") == "" {
			break
		}
	}
	if _, err := cc.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(cc, got); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("got %q", got)
	}
}
