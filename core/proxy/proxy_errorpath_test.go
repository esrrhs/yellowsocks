package proxy

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/esrrhs/gohome/network"
	"github.com/esrrhs/yellowsocks/core/router"
	"github.com/esrrhs/yellowsocks/core/stats"
)

// waitFor polls cond until it holds or the deadline passes, so assertions
// stay deterministic without fixed sleeps.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// closedPort returns a loopback address whose listener was closed, so dialing
// it is refused deterministically.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// splitPort splits "host:port" into its parts or fails the test.
func splitPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %s: %v", addr, err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatalf("port of %s: %v", addr, err)
	}
	return h, port
}

// directLoopbackRouter routes 127.0.0.1 targets direct. A server without a
// Router sends everything to the (SPP) upstream, so tests that exercise the
// direct paths must attach this.
func directLoopbackRouter(t *testing.T) *router.Router {
	t.Helper()
	r := router.NewRouter()
	t.Cleanup(r.Close)
	r.AddDirectDomain("127.0.0.1")
	return r
}

// startUDPEcho starts a loopback UDP echo server.
func startUDPEcho(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, rAddr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteToUDP(buf[:n], rAddr)
		}
	}()
	return conn
}

// recordingSmartUpstream is a SmartUpstreamProvider that also implements the
// failure-reporting and traffic-recording hooks, counting every call. The
// addr may point at a real mock SOCKS5 tunnel (successful sessions) or at a
// dead address (dial/resolve failures).
type recordingSmartUpstream struct {
	addr     string
	nodeName string

	pickNodeCalls int32
	nodeFailures  int32
	trafficUp     int64
	trafficDown   int64

	releaseCalled chan struct{}
	releaseOnce   sync.Once
}

func newRecordingSmartUpstream(addr, nodeName string) *recordingSmartUpstream {
	return &recordingSmartUpstream{addr: addr, nodeName: nodeName, releaseCalled: make(chan struct{})}
}

func (u *recordingSmartUpstream) Socks5Addr() string           { return u.addr }
func (u *recordingSmartUpstream) Socks5Auth() (string, string) { return "", "" }
func (u *recordingSmartUpstream) PickNode() (string, string, func()) {
	atomic.AddInt32(&u.pickNodeCalls, 1)
	return u.addr, u.nodeName, func() {
		u.releaseOnce.Do(func() { close(u.releaseCalled) })
	}
}
func (u *recordingSmartUpstream) ReportNodeFailure(_ string, _ error) {
	atomic.AddInt32(&u.nodeFailures, 1)
}
func (u *recordingSmartUpstream) RecordNodeTraffic(_ string, up, down int64) {
	atomic.AddInt64(&u.trafficUp, up)
	atomic.AddInt64(&u.trafficDown, down)
}

// plainReportingUpstream is a non-smart UpstreamProvider that only knows how
// to report a whole-channel failure.
type plainReportingUpstream struct {
	addr     string
	failures int32
}

func (u *plainReportingUpstream) Socks5Addr() string           { return u.addr }
func (u *plainReportingUpstream) Socks5Auth() (string, string) { return "", "" }
func (u *plainReportingUpstream) ReportFailure(_ error)        { atomic.AddInt32(&u.failures, 1) }

// startUpstreamClosingOnAccept runs a listener that closes every connection
// right after accepting, so the proxy's upstream SOCKS5 handshake fails.
func startUpstreamClosingOnAccept(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

// startUpstreamRejectingRequests accepts the SOCKS5 handshake but answers
// every CONNECT / UDP ASSOCIATE request with the given failure reply code.
func startUpstreamRejectingRequests(t *testing.T, rep byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if err := network.Sock5HandshakeBy(c, "", ""); err != nil {
					return
				}
				var head [4]byte
				if _, err := io.ReadFull(c, head[:]); err != nil {
					return
				}
				if _, _, err := readSocks5Addr(c, head[3]); err != nil {
					return
				}
				if err := network.Sock5SendConnectReply(c, rep, "0.0.0.0:0"); err != nil {
					return
				}
				// Hold the connection open until the proxy closes it so the
				// failure reply is never lost to a premature RST.
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				_, _ = c.Read(make([]byte, 1))
			}(c)
		}
	}()
	return ln.Addr().String()
}

// startUpstreamUDPRelay runs an SPP-like SOCKS5 upstream whose UDP ASSOCIATE
// binds a relay socket and echoes datagrams back through it, forwarding the
// payload to its real destination first. bndHost controls the BND.ADDR in the
// associate reply ("127.0.0.1" or the unspecified "0.0.0.0").
func startUpstreamUDPRelay(t *testing.T, bndHost string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if err := network.Sock5HandshakeBy(c, "", ""); err != nil {
					return
				}
				var head [4]byte
				if _, err := io.ReadFull(c, head[:]); err != nil {
					return
				}
				if head[1] != network.Socks5CmdUDPAssociate {
					return
				}
				if _, _, err := readSocks5Addr(c, head[3]); err != nil {
					return
				}
				uRelay, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
				if err != nil {
					return
				}
				defer uRelay.Close()
				relayPort := uRelay.LocalAddr().(*net.UDPAddr).Port
				if err := network.Sock5SendConnectReply(c, 0x00, net.JoinHostPort(bndHost, strconv.Itoa(relayPort))); err != nil {
					return
				}
				go func() {
					buf := make([]byte, 65535)
					for {
						n, src, err := uRelay.ReadFromUDP(buf)
						if err != nil {
							return
						}
						dstH, dstP, payload, err := network.Sock5UnpackUDP(buf[:n])
						if err != nil {
							continue
						}
						tAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(dstH, strconv.Itoa(dstP)))
						if err != nil {
							continue
						}
						fwd, err := net.ListenUDP("udp", nil)
						if err != nil {
							continue
						}
						if _, err := fwd.WriteToUDP(payload, tAddr); err != nil {
							_ = fwd.Close()
							continue
						}
						resp := make([]byte, 65535)
						_ = fwd.SetReadDeadline(time.Now().Add(2 * time.Second))
						rn, from, rErr := fwd.ReadFromUDP(resp)
						_ = fwd.Close()
						if rErr != nil || rn == 0 {
							continue
						}
						packed, err := network.Sock5PackUDP(from.IP.String(), from.Port, resp[:rn])
						if err != nil {
							continue
						}
						_, _ = uRelay.WriteToUDP(packed, src)
					}
				}()
				_, _ = c.Read(make([]byte, 1))
			}(c)
		}
	}()
	return ln.Addr().String()
}

// ---------------------------------------------------------------------------
// Server lifecycle: defaults, double start, port conflicts, idempotent Stop.
// ---------------------------------------------------------------------------

func TestSocks5ServerLifecycleEdges(t *testing.T) {
	s := NewSocks5Server(Socks5Config{})
	if s.cfg.ListenAddr != "127.0.0.1:1080" {
		t.Fatalf("default listen addr %q", s.cfg.ListenAddr)
	}
	if s.cfg.IdleTimeout != defaultIdleTimeout {
		t.Fatalf("default idle timeout %v", s.cfg.IdleTimeout)
	}
	if s.Addr() != nil {
		t.Fatal("Addr must be nil before Start")
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Stop() }()
	first := s.Addr().String()
	if err := s.Start(); err != nil {
		t.Fatalf("second Start must be a no-op, got %v", err)
	}
	if s.Addr().String() != first {
		t.Fatalf("second Start rebound the listener: %s != %s", s.Addr().String(), first)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("second Stop must be idempotent, got %v", err)
	}
	if s.Addr() != nil {
		t.Fatal("Addr must be nil after Stop")
	}
}

func TestSocks5ServerStartPortConflict(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	s := NewSocks5Server(Socks5Config{ListenAddr: ln.Addr().String()})
	if err := s.Start(); err == nil {
		_ = s.Stop()
		t.Fatal("Start must fail when the port is taken")
	} else if !strings.Contains(err.Error(), "socks5 listen failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHTTPServerLifecycleEdges(t *testing.T) {
	s := NewHTTPServer(HTTPConfig{})
	if s.cfg.ListenAddr != "127.0.0.1:8080" {
		t.Fatalf("default listen addr %q", s.cfg.ListenAddr)
	}
	if s.cfg.IdleTimeout != defaultIdleTimeout {
		t.Fatalf("default idle timeout %v", s.cfg.IdleTimeout)
	}
	if s.Addr() != nil {
		t.Fatal("Addr must be nil before Start")
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Stop() }()
	first := s.Addr().String()
	if err := s.Start(); err != nil {
		t.Fatalf("second Start must be a no-op, got %v", err)
	}
	if s.Addr().String() != first {
		t.Fatalf("second Start rebound the listener: %s != %s", s.Addr().String(), first)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("second Stop must be idempotent, got %v", err)
	}
	if s.Addr() != nil {
		t.Fatal("Addr must be nil after Stop")
	}
}

func TestHTTPServerStartPortConflict(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	s := NewHTTPServer(HTTPConfig{ListenAddr: ln.Addr().String()})
	if err := s.Start(); err == nil {
		_ = s.Stop()
		t.Fatal("Start must fail when the port is taken")
	} else if !strings.Contains(err.Error(), "http proxy listen failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// flakyListener fails Accept a fixed number of times before serving conns or
// closing, letting the test drive the acceptLoop retry branch.
type flakyListener struct {
	errsLeft  int32
	conns     chan net.Conn
	quit      chan struct{}
	closeOnce sync.Once
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if atomic.AddInt32(&l.errsLeft, -1) >= 0 {
		return nil, errors.New("transient accept failure")
	}
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.quit:
		return nil, errors.New("listener closed")
	}
}

func (l *flakyListener) Close() error {
	l.closeOnce.Do(func() { close(l.quit) })
	return nil
}

func (l *flakyListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1}
}

// A transient Accept error must be retried (not kill the loop), and the loop
// must still exit once the server is stopped.
func TestAcceptLoopsSurviveTransientAcceptErrors(t *testing.T) {
	t.Run("socks5", func(t *testing.T) {
		fl := &flakyListener{errsLeft: 2, conns: make(chan net.Conn, 1), quit: make(chan struct{})}
		s := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0"})
		exited := make(chan struct{})
		go func() {
			s.acceptLoop(fl)
			close(exited)
		}()
		waitFor(t, "transient accept errors consumed", func() bool { return atomic.LoadInt32(&fl.errsLeft) <= -1 })
		if err := s.Stop(); err != nil {
			t.Fatal(err)
		}
		_ = fl.Close()
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			t.Fatal("socks5 acceptLoop did not exit after Stop")
		}
	})

	t.Run("http", func(t *testing.T) {
		fl := &flakyListener{errsLeft: 2, conns: make(chan net.Conn, 1), quit: make(chan struct{})}
		s := NewHTTPServer(HTTPConfig{ListenAddr: "127.0.0.1:0"})
		exited := make(chan struct{})
		go func() {
			s.acceptLoop(fl)
			close(exited)
		}()
		waitFor(t, "transient accept errors consumed", func() bool { return atomic.LoadInt32(&fl.errsLeft) <= -1 })
		if err := s.Stop(); err != nil {
			t.Fatal(err)
		}
		_ = fl.Close()
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			t.Fatal("http acceptLoop did not exit after Stop")
		}
	})
}

// ---------------------------------------------------------------------------
// SOCKS5 handshake, auth and request-header error paths.
// ---------------------------------------------------------------------------

func TestSocks5AuthHandshakeRejections(t *testing.T) {
	echoAddr, stopEcho := startEchoTCP(t)
	defer stopEcho()

	s5 := NewSocks5Server(Socks5Config{
		ListenAddr: "127.0.0.1:0",
		Router:     directLoopbackRouter(t),
		Username:   "u",
		Password:   "p",
	})
	if err := s5.Start(); err != nil {
		t.Fatal(err)
	}
	defer s5.Stop()

	// 1. Only NoAuth offered -> 0xFF (no acceptable methods), then close.
	c, err := net.Dial("tcp", s5.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	rep := make([]byte, 2)
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatalf("read method reply: %v", err)
	}
	if rep[0] != 0x05 || rep[1] != 0xFF {
		t.Fatalf("method reply % x, want 05 ff", rep)
	}
	if _, err := c.Read(rep); err == nil {
		t.Fatal("connection must be closed after no acceptable method")
	}
	_ = c.Close()

	// 2. Wrong password -> RFC 1929 failure status, then close.
	c, err = net.Dial("tcp", s5.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte{0x05, 0x01, 0x02}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatalf("read method reply: %v", err)
	}
	if rep[1] != 0x02 {
		t.Fatalf("method reply % x, want 05 02", rep)
	}
	if _, err := c.Write([]byte{0x01, 0x01, 'u', 0x01, 'x'}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatalf("read auth reply: %v", err)
	}
	if rep[0] != 0x01 || rep[1] != 0x01 {
		t.Fatalf("auth reply % x, want 01 01", rep)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(rep); err == nil {
		t.Fatal("connection must be closed after auth failure")
	}
	_ = c.Close()

	// 3. Correct credentials -> CONNECT to the echo succeeds end to end.
	c, err = net.Dial("tcp", s5.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte{0x05, 0x01, 0x02}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatalf("read method reply: %v", err)
	}
	if _, err := c.Write([]byte{0x01, 0x01, 'u', 0x01, 'p'}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatalf("read auth reply: %v", err)
	}
	if rep[1] != 0x00 {
		t.Fatalf("auth reply % x, want success", rep)
	}
	host, port := splitPort(t, echoAddr)
	req := []byte{0x05, 0x01, 0x00, 0x01}
	req = append(req, net.ParseIP(host).To4()...)
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	full := make([]byte, 10)
	if _, err := io.ReadFull(c, full); err != nil {
		t.Fatalf("read connect reply: %v", err)
	}
	if full[1] != 0x00 {
		t.Fatalf("connect reply % x, want success", full)
	}
	msg := []byte("authenticated echo")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if string(got) != string(msg) {
		t.Fatalf("got %q", got)
	}
}

// A client that closes right after the handshake, or sends a request whose
// version byte is not 5, must end the session without a reply.
func TestSocks5RequestHeaderErrorsCloseConn(t *testing.T) {
	echoAddr, stopEcho := startEchoTCP(t)
	defer stopEcho()

	s5 := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0", Router: directLoopbackRouter(t)})
	if err := s5.Start(); err != nil {
		t.Fatal(err)
	}
	defer s5.Stop()

	// 1. EOF before the request header.
	c, err := net.Dial("tcp", s5.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := network.Sock5Handshake(c.(*net.TCPConn), 5000, "", ""); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()

	// 2. Request version byte 0x04 after a valid handshake.
	c, err = net.Dial("tcp", s5.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := network.Sock5Handshake(c.(*net.TCPConn), 5000, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte{0x04, 0x01, 0x00, 0x01}); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 8)); err == nil {
		t.Fatal("connection must be closed after a non-SOCKS5 request version")
	}

	// 3. The server is still healthy afterwards.
	if err := socks5ConnectRequest(t, s5.Addr().String(), splitHost(t, echoAddr), portNumberOf(t, echoAddr)); err != nil {
		t.Fatalf("server stopped serving after garbage requests: %v", err)
	}
}

func splitHost(t *testing.T, addr string) string {
	t.Helper()
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func portNumberOf(t *testing.T, addr string) int {
	t.Helper()
	_, port := splitPort(t, addr)
	return port
}

// readSocks5Addr must fail cleanly on truncated address bodies and ports.
func TestReadSocks5AddrTruncatedInputs(t *testing.T) {
	cases := []struct {
		name  string
		atyp  byte
		input []byte
	}{
		{"ipv4 truncated", 0x01, []byte{10, 0}},
		{"domain length missing", 0x03, nil},
		{"domain body missing", 0x03, []byte{9}},
		{"ipv6 truncated", 0x04, []byte{1, 2, 3, 4, 5}},
		{"port missing after ipv4", 0x01, []byte{10, 0, 0, 1}},
		{"port missing after domain", 0x03, []byte{3, 'a', 'b', 'c'}},
		{"port missing after ipv6", 0x04, make([]byte, 16)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := readSocks5Addr(bytes.NewReader(tc.input), tc.atyp)
			if err == nil {
				t.Fatal("expected an error for truncated input")
			}
		})
	}

	host, port, err := readSocks5Addr(bytes.NewReader([]byte{10, 1, 2, 3, 0x1F, 0x90}), 0x01)
	if err != nil || host != "10.1.2.3" || port != 8080 {
		t.Fatalf("ipv4: host=%q port=%d err=%v", host, port, err)
	}
	host, port, err = readSocks5Addr(bytes.NewReader(append([]byte{3, 'a', 'b', 'c'}, 0x00, 53)), 0x03)
	if err != nil || host != "abc" || port != 53 {
		t.Fatalf("domain: host=%q port=%d err=%v", host, port, err)
	}
	host, port, err = readSocks5Addr(bytes.NewReader(append(append([]byte{}, net.IPv6loopback.To16()...), 0x00, 53)), 0x04)
	if err != nil || host != "::1" || port != 53 {
		t.Fatalf("ipv6: host=%q port=%d err=%v", host, port, err)
	}
}

// ---------------------------------------------------------------------------
// SOCKS5 CONNECT error paths.
// ---------------------------------------------------------------------------

// socks5ConnectRequest dials the proxy, handshakes and issues a CONNECT,
// returning the error (if any) from the request round trip.
func socks5ConnectRequest(t *testing.T, proxyAddr, host string, port int) error {
	t.Helper()
	cc, err := net.DialTCP("tcp", nil, mustTCPAddr(t, proxyAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if err := network.Sock5Handshake(cc, 5000, "", ""); err != nil {
		t.Fatal(err)
	}
	return network.Sock5SetRequest(cc, host, port, 5000)
}

func TestSocks5ConnectDirectDialFailure(t *testing.T) {
	dead := closedPort(t)
	s5 := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0", Router: directLoopbackRouter(t)})
	if err := s5.Start(); err != nil {
		t.Fatal(err)
	}
	defer s5.Stop()

	host, port := splitPort(t, dead)
	if err := socks5ConnectRequest(t, s5.Addr().String(), host, port); err == nil {
		t.Fatal("expected a failure reply for a refused direct dial")
	}
}

func TestSocks5ConnectProxyRouteFailures(t *testing.T) {
	dead := closedPort(t)

	t.Run("no upstream available", func(t *testing.T) {
		s5 := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0", Router: proxyRouterWithLocalhost(t)})
		if err := s5.Start(); err != nil {
			t.Fatal(err)
		}
		defer s5.Stop()
		if err := socks5ConnectRequest(t, s5.Addr().String(), "localhost", 443); err == nil {
			t.Fatal("expected a failure reply with no upstream")
		}
	})

	t.Run("upstream address unresolvable", func(t *testing.T) {
		s5 := NewSocks5Server(Socks5Config{
			ListenAddr: "127.0.0.1:0",
			Router:     proxyRouterWithLocalhost(t),
			Upstream:   &mockUpstream{addr: "127.0.0.1:99999"},
		})
		if err := s5.Start(); err != nil {
			t.Fatal(err)
		}
		defer s5.Stop()
		if err := socks5ConnectRequest(t, s5.Addr().String(), "localhost", 443); err == nil {
			t.Fatal("expected a failure reply for an unresolvable upstream")
		}
	})

	t.Run("upstream dial refused with plain failure reporter", func(t *testing.T) {
		up := &plainReportingUpstream{addr: dead}
		s5 := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0", Router: proxyRouterWithLocalhost(t), Upstream: up})
		if err := s5.Start(); err != nil {
			t.Fatal(err)
		}
		defer s5.Stop()
		if err := socks5ConnectRequest(t, s5.Addr().String(), "localhost", 443); err == nil {
			t.Fatal("expected a failure reply for a refused upstream dial")
		}
		if atomic.LoadInt32(&up.failures) == 0 {
			t.Fatal("ReportFailure was not called")
		}
	})

	t.Run("upstream dial refused with smart node reporter", func(t *testing.T) {
		up := newRecordingSmartUpstream(dead, "node-a")
		s5 := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0", Router: proxyRouterWithLocalhost(t), Upstream: up})
		if err := s5.Start(); err != nil {
			t.Fatal(err)
		}
		defer s5.Stop()
		if err := socks5ConnectRequest(t, s5.Addr().String(), "localhost", 443); err == nil {
			t.Fatal("expected a failure reply for a refused upstream dial")
		}
		if atomic.LoadInt32(&up.nodeFailures) == 0 {
			t.Fatal("ReportNodeFailure was not called")
		}
	})

	t.Run("upstream closes before handshake", func(t *testing.T) {
		up := newRecordingSmartUpstream(startUpstreamClosingOnAccept(t), "node-b")
		s5 := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0", Router: proxyRouterWithLocalhost(t), Upstream: up})
		if err := s5.Start(); err != nil {
			t.Fatal(err)
		}
		defer s5.Stop()
		if err := socks5ConnectRequest(t, s5.Addr().String(), "localhost", 443); err == nil {
			t.Fatal("expected a failure reply for a failed upstream handshake")
		}
		if atomic.LoadInt32(&up.nodeFailures) == 0 {
			t.Fatal("ReportNodeFailure was not called for the handshake failure")
		}
	})

	t.Run("upstream closes before handshake with plain failure reporter", func(t *testing.T) {
		up := &plainReportingUpstream{addr: startUpstreamClosingOnAccept(t)}
		s5 := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0", Router: proxyRouterWithLocalhost(t), Upstream: up})
		if err := s5.Start(); err != nil {
			t.Fatal(err)
		}
		defer s5.Stop()
		if err := socks5ConnectRequest(t, s5.Addr().String(), "localhost", 443); err == nil {
			t.Fatal("expected a failure reply for a failed upstream handshake")
		}
		if atomic.LoadInt32(&up.failures) == 0 {
			t.Fatal("ReportFailure was not called for the handshake failure")
		}
	})

	t.Run("upstream rejects the connect request", func(t *testing.T) {
		up := startUpstreamRejectingRequests(t, 0x01)
		s5 := NewSocks5Server(Socks5Config{
			ListenAddr: "127.0.0.1:0",
			Router:     proxyRouterWithLocalhost(t),
			Upstream:   &mockUpstream{addr: up},
		})
		if err := s5.Start(); err != nil {
			t.Fatal(err)
		}
		defer s5.Stop()
		if err := socks5ConnectRequest(t, s5.Addr().String(), "localhost", 443); err == nil {
			t.Fatal("expected a failure reply when the upstream rejects the request")
		}
	})
}

// A successful CONNECT through a smart upstream with statistics must account
// traffic on the selected node and set it on the session.
func TestSocks5ConnectViaSmartUpstreamWithStats(t *testing.T) {
	upstreamAddr, stopUpstream := startMockUpstreamSocks5(t)
	defer stopUpstream()
	echoAddr, stopEcho := startEchoTCP(t)
	defer stopEcho()

	up := newRecordingSmartUpstream(upstreamAddr, "node-a")
	c := stats.New(stats.Config{HistoryMinutes: 60})
	defer c.Close()

	s5 := NewSocks5Server(Socks5Config{
		ListenAddr: "127.0.0.1:0",
		Router:     proxyRouterWithLocalhost(t),
		Upstream:   up,
		Stats:      c,
		ActiveNode: func() string { return up.nodeName },
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
	if err := network.Sock5SetRequest(cc, "localhost", port, 5000); err != nil {
		t.Fatalf("connect via smart upstream: %v", err)
	}
	msg := []byte("through the node")
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
	if atomic.LoadInt64(&up.trafficUp) == 0 || atomic.LoadInt64(&up.trafficDown) == 0 {
		t.Fatal("RecordNodeTraffic was not called with both directions")
	}
}

// ---------------------------------------------------------------------------
// SOCKS5 UDP ASSOCIATE branches.
// ---------------------------------------------------------------------------

// socks5UDPAssociate opens a TCP control connection, performs the handshake
// and a UDP ASSOCIATE, and returns the control conn, the relay address and
// the client's UDP socket. Cleanup closes both sockets.
func socks5UDPAssociate(t *testing.T, proxyAddr string) (net.Conn, *net.UDPAddr, *net.UDPConn) {
	t.Helper()
	cc, err := net.DialTCP("tcp", nil, mustTCPAddr(t, proxyAddr))
	if err != nil {
		t.Fatal(err)
	}
	if err := network.Sock5Handshake(cc, 5000, "", ""); err != nil {
		_ = cc.Close()
		t.Fatal(err)
	}
	relayStr, err := network.Sock5SetUDPRequest(cc, "0.0.0.0", 0, 5000)
	if err != nil {
		_ = cc.Close()
		t.Fatal(err)
	}
	relay, err := net.ResolveUDPAddr("udp", relayStr)
	if err != nil {
		_ = cc.Close()
		t.Fatal(err)
	}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		_ = cc.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cc.Close()
		_ = udp.Close()
	})
	return cc, relay, udp
}

func udpEchoRoundTrip(t *testing.T, udp *net.UDPConn, relay *net.UDPAddr, host string, port int, payload string) {
	t.Helper()
	pkt, err := network.Sock5PackUDP(host, port, []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := udp.WriteToUDP(pkt, relay); err != nil {
		t.Fatal(err)
	}
	_ = udp.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 65535)
	n, _, err := udp.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read echo for %q: %v", payload, err)
	}
	_, _, got, err := network.Sock5UnpackUDP(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatalf("echo payload %q, want %q", got, payload)
	}
}

// A datagram that is not valid SOCKS5 UDP framing must be dropped without
// tearing down the association.
func TestSocks5UDPGarbageDatagramKeepsAssociation(t *testing.T) {
	echo := startUDPEcho(t)
	host, port := splitPort(t, echo.LocalAddr().String())

	s5 := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0", Router: directLoopbackRouter(t)})
	if err := s5.Start(); err != nil {
		t.Fatal(err)
	}
	defer s5.Stop()

	_, relay, udp := socks5UDPAssociate(t, s5.Addr().String())

	if _, err := udp.WriteToUDP([]byte("not a socks5 datagram"), relay); err != nil {
		t.Fatal(err)
	}
	udpEchoRoundTrip(t, udp, relay, host, port, "after-garbage")
}

// A datagram for a proxy-routed IPv6 literal with IPv6 disabled must be
// dropped, and the association must survive.
func TestSocks5UDPIPv6LiteralOnProxyRouteDropped(t *testing.T) {
	echo := startUDPEcho(t)
	host, port := splitPort(t, echo.LocalAddr().String())

	s5 := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0", Router: proxyRouterWithLocalhost(t)})
	if err := s5.Start(); err != nil {
		t.Fatal(err)
	}
	defer s5.Stop()

	_, relay, udp := socks5UDPAssociate(t, s5.Addr().String())

	pkt, err := network.Sock5PackUDP("2607:f8b0:400a:801::200e", 53, []byte("v6"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := udp.WriteToUDP(pkt, relay); err != nil {
		t.Fatal(err)
	}
	udpEchoRoundTrip(t, udp, relay, host, port, "after-v6-drop")
}

// Every SPP UDP session creation failure must be contained: the association
// survives, direct datagrams keep flowing, and a picked upstream node is
// released.
func TestSocks5UDPSPPSessionFailures(t *testing.T) {
	dead := closedPort(t)
	echo := startUDPEcho(t)
	host, port := splitPort(t, echo.LocalAddr().String())

	cases := []struct {
		name     string
		upstream UpstreamProvider
	}{
		{"no upstream", nil},
		{"smart upstream with empty address", newRecordingSmartUpstream("", "node-e0")},
		{"upstream address unresolvable", newRecordingSmartUpstream("127.0.0.1:99999", "node-e1")},
		{"upstream dial refused", newRecordingSmartUpstream(dead, "node-e2")},
		{"upstream handshake fails", newRecordingSmartUpstream(startUpstreamClosingOnAccept(t), "node-e3")},
		{"upstream rejects associate", newRecordingSmartUpstream(startUpstreamRejectingRequests(t, 0x01), "node-e4")},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			s5 := NewSocks5Server(Socks5Config{
				ListenAddr: "127.0.0.1:0",
				Router:     proxyRouterWithLocalhost(t),
				Upstream:   tc.upstream,
			})
			if err := s5.Start(); err != nil {
				t.Fatal(err)
			}
			defer s5.Stop()

			_, relay, udp := socks5UDPAssociate(t, s5.Addr().String())

			// Proxy-routed target hits the SPP UDP path, which fails.
			pkt, err := network.Sock5PackUDP("8.8.8.8", 53, []byte("q"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := udp.WriteToUDP(pkt, relay); err != nil {
				t.Fatal(err)
			}
			// The association must survive the failure.
			udpEchoRoundTrip(t, udp, relay, host, port, "still-alive")

			// A smart upstream picked (and must then release) a node while
			// trying to build the failed session.
			if smart, ok := tc.upstream.(*recordingSmartUpstream); ok {
				select {
				case <-smart.releaseCalled:
				default:
					t.Fatal("upstream release was not called for the failed SPP session")
				}
			}
		})
	}
}

// A full UDP round trip through the SPP relay: the session is created on the
// first datagram, reused for the second, replies are forwarded back, and
// closing the control connection releases the upstream node.
func TestSocks5UDPRelayViaSPPRoundTrip(t *testing.T) {
	upstreamAddr := startUpstreamUDPRelay(t, "0.0.0.0")
	echo := startUDPEcho(t)
	_, echoPort := splitPort(t, echo.LocalAddr().String())

	smart := newRecordingSmartUpstream(upstreamAddr, "udp-node")
	c := stats.New(stats.Config{HistoryMinutes: 60})
	defer c.Close()

	s5 := NewSocks5Server(Socks5Config{
		ListenAddr: "127.0.0.1:0",
		Router:     proxyRouterWithLocalhost(t),
		Upstream:   smart,
		Stats:      c,
		ActiveNode: func() string { return smart.nodeName },
	})
	if err := s5.Start(); err != nil {
		t.Fatal(err)
	}
	defer s5.Stop()

	cc, relay, udp := socks5UDPAssociate(t, s5.Addr().String())

	// "localhost" is proxy-routed, so the datagrams traverse the mock SPP;
	// the mock forwards them to the loopback echo.
	for i := 0; i < 2; i++ {
		payload := fmt.Sprintf("dns-q-%d", i)
		pkt, err := network.Sock5PackUDP("localhost", echoPort, []byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := udp.WriteToUDP(pkt, relay); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		_ = udp.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 65535)
		n, _, err := udp.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("read SPP relayed reply %d: %v", i, err)
		}
		_, _, got, err := network.Sock5UnpackUDP(buf[:n])
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(got), "dns-q-") {
			t.Fatalf("unexpected relayed payload %q", got)
		}
	}

	// Closing the control connection ends the association and must release
	// the picked upstream node (closeSPP).
	_ = cc.Close()
	select {
	case <-smart.releaseCalled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream release was not called on association teardown")
	}
}

// Direct UDP sessions are capped per association; datagrams beyond the cap
// are dropped but the association and the cached sessions keep working.
func TestSocks5UDPDirectSessionCapKeepsAssociationAlive(t *testing.T) {
	echo := startUDPEcho(t)
	host, echoPort := splitPort(t, echo.LocalAddr().String())

	s5 := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0", Router: directLoopbackRouter(t)})
	if err := s5.Start(); err != nil {
		t.Fatal(err)
	}
	defer s5.Stop()

	_, relay, udp := socks5UDPAssociate(t, s5.Addr().String())

	// First target is the echo: it stays cached for the whole association.
	if err := sendDirectUDP(t, udp, relay, host, echoPort, "first"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < maxDirectUDPSessions; i++ {
		if err := sendDirectUDP(t, udp, relay, host, 20000+i, "filler"); err != nil {
			t.Fatal(err)
		}
	}
	// One datagram beyond the cap must be dropped without killing anything.
	if err := sendDirectUDP(t, udp, relay, host, 1, "over-cap"); err != nil {
		t.Fatal(err)
	}

	// The cached echo session still relays in both directions.
	if err := sendDirectUDP(t, udp, relay, host, echoPort, "second"); err != nil {
		t.Fatal(err)
	}
	_ = udp.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 65535)
	for {
		n, _, err := udp.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("echo after cap exhausted: %v", err)
		}
		_, _, got, err := network.Sock5UnpackUDP(buf[:n])
		if err != nil {
			t.Fatal(err)
		}
		if string(got) == "second" {
			break
		}
		// Earlier echoes ("first") may still be queued; keep draining.
	}
}

func sendDirectUDP(t *testing.T, udp *net.UDPConn, relay *net.UDPAddr, host string, port int, payload string) error {
	t.Helper()
	pkt, err := network.Sock5PackUDP(host, port, []byte(payload))
	if err != nil {
		return err
	}
	_, err = udp.WriteToUDP(pkt, relay)
	return err
}

// ---------------------------------------------------------------------------
// HTTP proxy error paths.
// ---------------------------------------------------------------------------

// httpRawRequest sends a raw request head to the proxy and parses the reply.
func httpRawRequest(t *testing.T, proxyAddr, req string) *http.Response {
	t.Helper()
	cc, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if _, err := cc.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	_ = cc.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(cc), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp
}

func TestHTTPCheckAuth(t *testing.T) {
	s := NewHTTPServer(HTTPConfig{Username: "user", Password: "pass"})
	if !s.checkAuth("Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass"))) {
		t.Fatal("valid credentials rejected")
	}
	if s.checkAuth("") {
		t.Fatal("empty auth accepted")
	}
	if s.checkAuth("Bearer " + base64.StdEncoding.EncodeToString([]byte("user:pass"))) {
		t.Fatal("non-Basic scheme accepted")
	}
	if s.checkAuth("Basic !!!!not-base64!!!!") {
		t.Fatal("invalid base64 accepted")
	}
	if s.checkAuth("Basic " + base64.StdEncoding.EncodeToString([]byte("user:wrong"))) {
		t.Fatal("wrong password accepted")
	}
	if s.checkAuth("Basic " + base64.StdEncoding.EncodeToString([]byte("user"))) {
		t.Fatal("credentials without a colon accepted")
	}

	open := NewHTTPServer(HTTPConfig{})
	if !open.checkAuth("") {
		t.Fatal("open proxy must accept an empty auth header")
	}
	if !open.checkAuth("Basic anything") {
		t.Fatal("open proxy must accept any auth header")
	}
}

func TestHTTPParseTarget(t *testing.T) {
	s := NewHTTPServer(HTTPConfig{})
	if h, p := s.parseTarget("CONNECT", "example.com", ""); h != "example.com" || p != 443 {
		t.Fatalf("connect without port: %q:%d", h, p)
	}
	if h, p := s.parseTarget("CONNECT", "host.example:8443", ""); h != "host.example" || p != 8443 {
		t.Fatalf("connect with port: %q:%d", h, p)
	}
	if h, p := s.parseTarget("CONNECT", "host.example:notaport", ""); h != "" || p != 0 {
		t.Fatalf("connect with bad port: %q:%d", h, p)
	}
	if h, p := s.parseTarget("GET", "http://host.example", ""); h != "host.example" || p != 80 {
		t.Fatalf("absolute uri without path: %q:%d", h, p)
	}
	if h, p := s.parseTarget("GET", "/path", "host.example:8080"); h != "host.example" || p != 8080 {
		t.Fatalf("origin form with host header: %q:%d", h, p)
	}
}

// CONNECT without a port falls back to 443; dialing the refused loopback
// port must yield 502.
func TestHTTPConnectTargetWithoutPortDialsDefault(t *testing.T) {
	if c, err := net.DialTimeout("tcp", "127.0.0.1:443", 250*time.Millisecond); err == nil {
		_ = c.Close()
		t.Skip("something is listening on 127.0.0.1:443")
	}
	r := router.NewRouter()
	defer r.Close()
	hp := NewHTTPServer(HTTPConfig{ListenAddr: "127.0.0.1:0", Router: r})
	if err := hp.Start(); err != nil {
		t.Fatal(err)
	}
	defer hp.Stop()

	resp := httpRawRequest(t, hp.Addr().String(), "CONNECT 127.0.0.1 HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

// An absolute-form URI without a path is rewritten to "/" on the wire.
func TestHTTPGetAbsoluteURIWithoutPath(t *testing.T) {
	backend, getRaw := startRawHTTPBackend(t)
	r := router.NewRouter()
	defer r.Close()
	hp := NewHTTPServer(HTTPConfig{ListenAddr: "127.0.0.1:0", Router: r})
	if err := hp.Start(); err != nil {
		t.Fatal(err)
	}
	defer hp.Stop()

	resp := httpRawRequest(t, hp.Addr().String(),
		fmt.Sprintf("GET http://%s HTTP/1.1\r\nHost: %s\r\n\r\n", backend, backend))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if raw := getRaw(); !strings.HasPrefix(raw, "GET / HTTP/1.1\r\n") {
		t.Fatalf("absolute URI without path not rewritten to origin form:\n%s", raw)
	}
}

func TestHTTPGetDirectDialFailure(t *testing.T) {
	dead := closedPort(t)
	r := router.NewRouter()
	defer r.Close()
	hp := NewHTTPServer(HTTPConfig{ListenAddr: "127.0.0.1:0", Router: r})
	if err := hp.Start(); err != nil {
		t.Fatal(err)
	}
	defer hp.Stop()

	resp := httpRawRequest(t, hp.Addr().String(),
		fmt.Sprintf("GET http://%s/ HTTP/1.1\r\nHost: %s\r\n\r\n", dead, dead))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

// A proxy-routed IPv6 literal with IPv6 disabled must be answered with 502
// before any upstream is contacted.
func TestHTTPGetIPv6LiteralProxyRouteRejected(t *testing.T) {
	r := router.NewRouter()
	defer r.Close()
	hp := NewHTTPServer(HTTPConfig{ListenAddr: "127.0.0.1:0", Router: r})
	if err := hp.Start(); err != nil {
		t.Fatal(err)
	}
	defer hp.Stop()

	resp := httpRawRequest(t, hp.Addr().String(),
		"GET http://[2607:f8b0:400a:801::200e]:53/ HTTP/1.1\r\nHost: [2607:f8b0:400a:801::200e]:53\r\n\r\n")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestHTTPGetProxyRouteFailures(t *testing.T) {
	cases := []struct {
		name     string
		upstream UpstreamProvider
	}{
		{"no upstream", nil},
		{"upstream address unresolvable", &mockUpstream{addr: "127.0.0.1:99999"}},
		{"upstream handshake fails", &mockUpstream{addr: startUpstreamClosingOnAccept(t)}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			hp := NewHTTPServer(HTTPConfig{
				ListenAddr: "127.0.0.1:0",
				Router:     proxyRouterWithLocalhost(t),
				Upstream:   tc.upstream,
			})
			if err := hp.Start(); err != nil {
				t.Fatal(err)
			}
			defer hp.Stop()

			resp := httpRawRequest(t, hp.Addr().String(),
				"GET http://localhost:443/ HTTP/1.1\r\nHost: localhost:443\r\n\r\n")
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", resp.StatusCode)
			}
		})
	}
}

func TestHTTPConnectProxyRouteFailures(t *testing.T) {
	dead := closedPort(t)

	cases := []struct {
		name     string
		upstream UpstreamProvider
	}{
		{"no upstream", nil},
		{"upstream address unresolvable", &mockUpstream{addr: "127.0.0.1:99999"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			hp := NewHTTPServer(HTTPConfig{
				ListenAddr: "127.0.0.1:0",
				Router:     proxyRouterWithLocalhost(t),
				Upstream:   tc.upstream,
			})
			if err := hp.Start(); err != nil {
				t.Fatal(err)
			}
			defer hp.Stop()

			resp := httpRawRequest(t, hp.Addr().String(),
				"CONNECT localhost:443 HTTP/1.1\r\nHost: localhost:443\r\n\r\n")
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", resp.StatusCode)
			}
		})
	}

	t.Run("upstream closes before handshake with smart reporter", func(t *testing.T) {
		up := newRecordingSmartUpstream(startUpstreamClosingOnAccept(t), "node-h")
		hp := NewHTTPServer(HTTPConfig{
			ListenAddr: "127.0.0.1:0",
			Router:     proxyRouterWithLocalhost(t),
			Upstream:   up,
		})
		if err := hp.Start(); err != nil {
			t.Fatal(err)
		}
		defer hp.Stop()

		resp := httpRawRequest(t, hp.Addr().String(),
			"CONNECT localhost:443 HTTP/1.1\r\nHost: localhost:443\r\n\r\n")
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
		if atomic.LoadInt32(&up.nodeFailures) == 0 {
			t.Fatal("ReportNodeFailure was not called for the handshake failure")
		}
	})

	t.Run("upstream rejects the connect request", func(t *testing.T) {
		up := startUpstreamRejectingRequests(t, 0x01)
		hp := NewHTTPServer(HTTPConfig{
			ListenAddr: "127.0.0.1:0",
			Router:     proxyRouterWithLocalhost(t),
			Upstream:   &mockUpstream{addr: up},
		})
		if err := hp.Start(); err != nil {
			t.Fatal(err)
		}
		defer hp.Stop()

		resp := httpRawRequest(t, hp.Addr().String(),
			"CONNECT localhost:443 HTTP/1.1\r\nHost: localhost:443\r\n\r\n")
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
	})

	t.Run("upstream closes before handshake with plain failure reporter", func(t *testing.T) {
		up := &plainReportingUpstream{addr: startUpstreamClosingOnAccept(t)}
		hp := NewHTTPServer(HTTPConfig{
			ListenAddr: "127.0.0.1:0",
			Router:     proxyRouterWithLocalhost(t),
			Upstream:   up,
		})
		if err := hp.Start(); err != nil {
			t.Fatal(err)
		}
		defer hp.Stop()

		resp := httpRawRequest(t, hp.Addr().String(),
			"CONNECT localhost:443 HTTP/1.1\r\nHost: localhost:443\r\n\r\n")
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
		if atomic.LoadInt32(&up.failures) == 0 {
			t.Fatal("ReportFailure was not called for the handshake failure")
		}
	})

	t.Run("upstream dial refused with smart node reporter", func(t *testing.T) {
		up := newRecordingSmartUpstream(dead, "node-g")
		hp := NewHTTPServer(HTTPConfig{
			ListenAddr: "127.0.0.1:0",
			Router:     proxyRouterWithLocalhost(t),
			Upstream:   up,
		})
		if err := hp.Start(); err != nil {
			t.Fatal(err)
		}
		defer hp.Stop()

		resp := httpRawRequest(t, hp.Addr().String(),
			"CONNECT localhost:443 HTTP/1.1\r\nHost: localhost:443\r\n\r\n")
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
		if atomic.LoadInt32(&up.nodeFailures) == 0 {
			t.Fatal("ReportNodeFailure was not called for the dial failure")
		}
	})

	t.Run("upstream dial refused with plain failure reporter", func(t *testing.T) {
		up := &plainReportingUpstream{addr: dead}
		hp := NewHTTPServer(HTTPConfig{
			ListenAddr: "127.0.0.1:0",
			Router:     proxyRouterWithLocalhost(t),
			Upstream:   up,
		})
		if err := hp.Start(); err != nil {
			t.Fatal(err)
		}
		defer hp.Stop()

		resp := httpRawRequest(t, hp.Addr().String(),
			"CONNECT localhost:443 HTTP/1.1\r\nHost: localhost:443\r\n\r\n")
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
		if atomic.LoadInt32(&up.failures) == 0 {
			t.Fatal("ReportFailure was not called")
		}
	})
}

// A GET through a smart upstream with statistics must attribute the node and
// forward the request to the far end.
func TestHTTPGetViaSmartUpstreamWithStats(t *testing.T) {
	upstreamAddr, stopUpstream := startMockUpstreamSocks5(t)
	defer stopUpstream()
	backend, getRaw := startRawHTTPBackend(t)
	_, backendPort := splitPort(t, backend)

	up := newRecordingSmartUpstream(upstreamAddr, "http-node")
	c := stats.New(stats.Config{HistoryMinutes: 60})
	defer c.Close()

	hp := NewHTTPServer(HTTPConfig{
		ListenAddr: "127.0.0.1:0",
		Router:     proxyRouterWithLocalhost(t),
		Upstream:   up,
		Stats:      c,
		ActiveNode: func() string { return up.nodeName },
	})
	if err := hp.Start(); err != nil {
		t.Fatal(err)
	}
	defer hp.Stop()

	target := net.JoinHostPort("localhost", strconv.Itoa(backendPort))
	resp := httpRawRequest(t, hp.Addr().String(),
		fmt.Sprintf("GET http://%s/path HTTP/1.1\r\nHost: %s\r\n\r\n", target, target))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw := getRaw()
	if !strings.HasPrefix(raw, "GET /path HTTP/1.1\r\n") {
		t.Fatalf("request line not rewritten:\n%s", raw)
	}
	if atomic.LoadInt64(&up.trafficUp) == 0 || atomic.LoadInt64(&up.trafficDown) == 0 {
		t.Fatal("RecordNodeTraffic was not called with both directions")
	}
}

// ---------------------------------------------------------------------------
// Small helper units.
// ---------------------------------------------------------------------------

func TestReadLimitedLineReturnsEOFWithPartialData(t *testing.T) {
	remaining := 64
	line, err := readLimitedLine(bufio.NewReader(strings.NewReader("no newline")), &remaining)
	if err == nil {
		t.Fatal("expected io.EOF")
	}
	if line != "no newline" {
		t.Fatalf("line %q", line)
	}
	if remaining != 64-len("no newline") {
		t.Fatalf("budget not charged: %d", remaining)
	}
}

// noHalfCloseConn forwards net.Conn but hides CloseWrite, exercising the
// plain-Close fallback in the relay and prefixed reader.
type noHalfCloseConn struct {
	c net.Conn
}

func (p *noHalfCloseConn) Read(b []byte) (int, error)         { return p.c.Read(b) }
func (p *noHalfCloseConn) Write(b []byte) (int, error)        { return p.c.Write(b) }
func (p *noHalfCloseConn) Close() error                       { return p.c.Close() }
func (p *noHalfCloseConn) LocalAddr() net.Addr                { return p.c.LocalAddr() }
func (p *noHalfCloseConn) RemoteAddr() net.Addr               { return p.c.RemoteAddr() }
func (p *noHalfCloseConn) SetDeadline(t time.Time) error      { return p.c.SetDeadline(t) }
func (p *noHalfCloseConn) SetReadDeadline(t time.Time) error  { return p.c.SetReadDeadline(t) }
func (p *noHalfCloseConn) SetWriteDeadline(t time.Time) error { return p.c.SetWriteDeadline(t) }

func TestPrefixedReaderConnCloseWriteFallsBackToClose(t *testing.T) {
	ta, tb := loopbackPair(t)
	defer ta.Close()
	defer tb.Close()

	p := &prefixedReaderConn{Conn: &noHalfCloseConn{c: tb}}
	if err := p.CloseWrite(); err != nil {
		t.Fatalf("fallback CloseWrite: %v", err)
	}
	// The whole underlying connection is closed: the peer's writes must
	// eventually fail once the reset comes back.
	waitFor(t, "peer write failure after fallback close", func() bool {
		_, err := ta.Write([]byte("x"))
		return err != nil
	})
}

// loopbackPair returns two ends of a loopback TCP connection.
func loopbackPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		_ = client.Close()
		t.Fatal(r.err)
	}
	return client, r.c
}

// A relay whose sockets do not implement CloseWrite must fall back to a full
// Close on both ends when a direction ends.
func TestRelayStreamsClosesConnsWithoutCloseWrite(t *testing.T) {
	ta, ra := loopbackPair(t)
	defer ta.Close()
	tb, rb := loopbackPair(t)
	defer tb.Close()

	done := make(chan struct{})
	go func() {
		relayStreams(&noHalfCloseConn{c: ra}, &noHalfCloseConn{c: rb}, defaultIdleTimeout, nil)
		close(done)
	}()

	msg := []byte("through the relay")
	if _, err := ta.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(tb, got); err != nil {
		t.Fatalf("read through relay: %v", err)
	}
	if string(got) != string(msg) {
		t.Fatalf("got %q", got)
	}

	// EOF from the client end tears the relay down and closes both ends
	// (CloseWrite is unavailable, so a full Close must happen).
	_ = ta.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not finish after client EOF")
	}
	waitFor(t, "relay closed the target end", func() bool {
		_ = tb.SetReadDeadline(time.Now().Add(time.Second))
		_, err := tb.Read(make([]byte, 1))
		return err != nil
	})
}

// A read timeout on one direction must not kill the relay while the opposite
// direction is actively transferring: the deadline is refreshed and the
// silent side resumes.
func TestRelayReadTimeoutContinuesWhileOtherDirectionActive(t *testing.T) {
	const lines = 15

	// Side b: a server that streams data and ignores whatever it receives.
	tb, rb := loopbackPair(t)
	defer tb.Close()
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		go func() { _, _ = io.Copy(io.Discard, tb) }()
		for i := 0; i < lines; i++ {
			if _, err := tb.Write([]byte("DATA\n")); err != nil {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
	}()

	// Side a: a client that sends one byte and then stays silent.
	ta, ra := loopbackPair(t)
	defer ta.Close()
	done := make(chan struct{})
	go func() {
		relayStreams(ra, rb, 300*time.Millisecond, nil)
		close(done)
	}()

	if _, err := ta.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	_ = ta.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(ta)
	for i := 0; i < lines; i++ {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("one-way stream cut at line %d: %v", i, err)
		}
		if line != "DATA\n" {
			t.Fatalf("chunk %q", line)
		}
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not finish after the stream ended")
	}
	if _, err := ta.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected the relay to close the client side")
	}
	<-streamDone
}

// ---------------------------------------------------------------------------
// Address helpers.
// ---------------------------------------------------------------------------

func TestAddrHelpersEdgeCases(t *testing.T) {
	if got := addrString(nil); got != "" {
		t.Fatalf("addrString(nil) = %q", got)
	}
	if !sameUDPAddr(nil, nil) {
		t.Fatal("sameUDPAddr(nil, nil) must be true")
	}
	if sameUDPAddr(nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1}) {
		t.Fatal("sameUDPAddr(nil, addr) must be false")
	}
	if sameUDPAddr(&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1}, nil) {
		t.Fatal("sameUDPAddr(addr, nil) must be false")
	}
	if got := canonicalHost("[2001:db8::1]"); got != "2001:db8::1" {
		t.Fatalf("canonicalHost bracketed v6 = %q", got)
	}
	if got := canonicalHost("[127.0.0.1]"); got != "127.0.0.1" {
		t.Fatalf("canonicalHost bracketed v4 = %q", got)
	}
	if !isIPv6Literal("[2001:db8::1]") {
		t.Fatal("bracketed IPv6 literal not detected")
	}
	if isIPv6Literal("[127.0.0.1]") {
		t.Fatal("bracketed IPv4 detected as IPv6")
	}
}

func TestDirectUDPRegistryResolveError(t *testing.T) {
	r := newDirectUDPRegistry()
	if _, _, err := r.get("127.0.0.1"); err == nil {
		t.Fatal("expected a resolve error for an address without a port")
	}
	r.closeAll()
}

// listenClientUDPRelay must bind an IPv6 relay when the peer address cannot
// be parsed but the local address is IPv6.
func TestListenClientUDPRelayNilRemoteIPv6Local(t *testing.T) {
	conn, bnd, err := listenClientUDPRelay(&fakeRelayConn{
		local:  &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 1080},
		remote: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	host, _, err := net.SplitHostPort(bnd)
	if err != nil {
		t.Fatalf("bnd %q: %v", bnd, err)
	}
	if host != "2001:db8::1" {
		t.Fatalf("relay reply host %q, want the local IPv6", host)
	}
	if net.ParseIP(host).To4() != nil {
		t.Fatalf("relay bound to IPv4 reply %q", bnd)
	}
}
