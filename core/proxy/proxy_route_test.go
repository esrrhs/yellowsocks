package proxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/esrrhs/gohome/network"
	"github.com/esrrhs/yellowsocks/core/router"
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
	backend := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.RequestURI == "" || r.RequestURI[0] != '/' {
			http.Error(w, "expected origin-form URI", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("upstream-body"))
	})}
	bl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer bl.Close()
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
	target := net.JoinHostPort("localhost", portOf(t, bl.Addr().String()))
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
