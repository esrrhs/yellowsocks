package proxy

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/esrrhs/gohome/network"
	"github.com/esrrhs/yellowsocks/core/router"
)

type mockUpstream struct {
	addr string
}

func (m *mockUpstream) Socks5Addr() string {
	return m.addr
}

func (m *mockUpstream) Socks5Auth() (string, string) {
	return "", ""
}

// Start a lightweight mock upstream SOCKS5 server supporting CONNECT and UDP ASSOCIATE
func startMockUpstreamSocks5(t *testing.T) (string, func()) {
	tcpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen mock upstream tcp: %v", err)
	}

	stopCh := make(chan struct{})

	go func() {
		for {
			conn, err := tcpL.Accept()
			if err != nil {
				select {
				case <-stopCh:
					return
				default:
					return
				}
			}

			go func(c net.Conn) {
				defer c.Close()
				_ = network.Sock5HandshakeBy(c, "", "")

				var head [4]byte
				if _, err := io.ReadFull(c, head[:]); err != nil {
					return
				}
				cmd := head[1]
				atyp := head[3]
				host, port, err := readSocks5Addr(c, atyp)
				if err != nil {
					return
				}

				if cmd == network.Socks5CmdConnect {
					target := net.JoinHostPort(host, strconv.Itoa(port))
					dstConn, err := net.DialTimeout("tcp", target, 5*time.Second)
					if err != nil {
						_ = network.Sock5SendConnectReply(c, 0x05, "0.0.0.0:0")
						return
					}
					defer dstConn.Close()
					_ = network.Sock5SendConnectReply(c, 0x00, "0.0.0.0:0")
					relayStreams(c, dstConn, defaultIdleTimeout, nil)
				} else if cmd == network.Socks5CmdUDPAssociate {
					uRelay, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
					if err != nil {
						return
					}
					defer uRelay.Close()

					relayPort := uRelay.LocalAddr().(*net.UDPAddr).Port
					_ = network.Sock5SendConnectReply(c, 0x00, net.JoinHostPort("127.0.0.1", strconv.Itoa(relayPort)))

					go func() {
						buf := make([]byte, 65535)
						for {
							n, clientSrc, err := uRelay.ReadFromUDP(buf)
							if err != nil {
								return
							}
							dstH, dstP, payload, err := network.Sock5UnpackUDP(buf[:n])
							if err != nil {
								continue
							}
							// Send to target
							tDst, err := net.ResolveUDPAddr("udp", net.JoinHostPort(dstH, strconv.Itoa(dstP)))
							if err != nil {
								continue
							}
							fwdConn, err := net.ListenUDP("udp", nil)
							if err != nil {
								continue
							}
							_, _ = fwdConn.WriteToUDP(payload, tDst)
							// Read response from target
							respBuf := make([]byte, 65535)
							_ = fwdConn.SetReadDeadline(time.Now().Add(2 * time.Second))
							rn, fromAddr, rErr := fwdConn.ReadFromUDP(respBuf)
							fwdConn.Close()
							if rErr == nil && rn > 0 {
								packed, _ := network.Sock5PackUDP(fromAddr.IP.String(), fromAddr.Port, respBuf[:rn])
								_, _ = uRelay.WriteToUDP(packed, clientSrc)
							}
						}
					}()

					// Wait for TCP close
					dummy := make([]byte, 1)
					_, _ = c.Read(dummy)
				}
			}(conn)
		}
	}()

	cleanup := func() {
		close(stopCh)
		_ = tcpL.Close()
	}

	return tcpL.Addr().String(), cleanup
}

func TestSocks5TCP_DirectAndProxy(t *testing.T) {
	// 1. Start echo TCP server
	echoL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen failed: %v", err)
	}
	defer echoL.Close()
	echoAddr := echoL.Addr().String()

	go func() {
		for {
			conn, err := echoL.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	// 2. Start mock upstream SPP
	upstreamAddr, cleanupUpstream := startMockUpstreamSocks5(t)
	defer cleanupUpstream()

	// 3. Configure router: 127.0.0.1 is Direct, external.example is Proxy
	r := router.NewRouter()
	defer r.Close()
	r.AddProxyDomain("proxy.example")

	// 4. Start YellowSocks SOCKS5 Server
	s5Server := NewSocks5Server(Socks5Config{
		ListenAddr: "127.0.0.1:0",
		Router:     r,
		Upstream:   &mockUpstream{addr: upstreamAddr},
	})
	if err := s5Server.Start(); err != nil {
		t.Fatalf("s5 start failed: %v", err)
	}
	defer s5Server.Stop()

	s5Addr := s5Server.Addr().String()

	// Test Direct TCP Connect (127.0.0.1:echoPort)
	s5TCPAddr, err := net.ResolveTCPAddr("tcp", s5Addr)
	if err != nil {
		t.Fatalf("resolve s5 failed: %v", err)
	}
	clientConn, err := net.DialTCP("tcp", nil, s5TCPAddr)
	if err != nil {
		t.Fatalf("dial s5 failed: %v", err)
	}
	defer clientConn.Close()

	if err := network.Sock5Handshake(clientConn, 5000, "", ""); err != nil {
		t.Fatalf("client s5 handshake failed: %v", err)
	}

	echoHost, echoPortStr, _ := net.SplitHostPort(echoAddr)
	echoPort, _ := strconv.Atoi(echoPortStr)

	if err := network.Sock5SetRequest(clientConn, echoHost, echoPort, 5000); err != nil {
		t.Fatalf("client s5 connect failed: %v", err)
	}

	msg := []byte("hello yellowsocks direct")
	if _, err := clientConn.Write(msg); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	reply := make([]byte, len(msg))
	if _, err := io.ReadFull(clientConn, reply); err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(reply) != string(msg) {
		t.Errorf("expected %s, got %s", msg, reply)
	}
}

func TestSocks5UDP_DirectAndProxy(t *testing.T) {
	// 1. Start echo UDP server
	udpEcho, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("udp echo listen failed: %v", err)
	}
	defer udpEcho.Close()
	echoAddr := udpEcho.LocalAddr().String()

	go func() {
		buf := make([]byte, 2048)
		for {
			n, rAddr, err := udpEcho.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = udpEcho.WriteToUDP(buf[:n], rAddr)
		}
	}()

	upstreamAddr, cleanupUpstream := startMockUpstreamSocks5(t)
	defer cleanupUpstream()

	r := router.NewRouter()
	defer r.Close()

	s5Server := NewSocks5Server(Socks5Config{
		ListenAddr: "127.0.0.1:0",
		Router:     r,
		Upstream:   &mockUpstream{addr: upstreamAddr},
	})
	if err := s5Server.Start(); err != nil {
		t.Fatalf("s5 start failed: %v", err)
	}
	defer s5Server.Stop()

	// Client dials SOCKS5 server for UDP associate
	s5TCPAddr, err := net.ResolveTCPAddr("tcp", s5Server.Addr().String())
	if err != nil {
		t.Fatalf("resolve s5 failed: %v", err)
	}
	tcpConn, err := net.DialTCP("tcp", nil, s5TCPAddr)
	if err != nil {
		t.Fatalf("dial s5 failed: %v", err)
	}
	defer tcpConn.Close()

	if err := network.Sock5Handshake(tcpConn, 5000, "", ""); err != nil {
		t.Fatalf("handshake failed: %v", err)
	}

	relayAddrStr, err := network.Sock5SetUDPRequest(tcpConn, "0.0.0.0", 0, 5000)
	if err != nil {
		t.Fatalf("set udp request failed: %v", err)
	}

	relayUDP, err := net.ResolveUDPAddr("udp", relayAddrStr)
	if err != nil {
		t.Fatalf("resolve relay addr failed: %v", err)
	}

	clientUDP, err := net.ListenUDP("udp", nil)
	if err != nil {
		t.Fatalf("client udp listen failed: %v", err)
	}
	defer clientUDP.Close()

	echoHost, echoPortStr, _ := net.SplitHostPort(echoAddr)
	echoPort, _ := strconv.Atoi(echoPortStr)

	payload := []byte("udp ping test")
	packedPkt, err := network.Sock5PackUDP(echoHost, echoPort, payload)
	if err != nil {
		t.Fatalf("pack udp failed: %v", err)
	}

	if _, err := clientUDP.WriteToUDP(packedPkt, relayUDP); err != nil {
		t.Fatalf("write to relay failed: %v", err)
	}

	respBuf := make([]byte, 65535)
	_ = clientUDP.SetReadDeadline(time.Now().Add(3 * time.Second))
	rn, _, err := clientUDP.ReadFromUDP(respBuf)
	if err != nil {
		t.Fatalf("read from relay failed: %v", err)
	}

	_, _, respPayload, err := network.Sock5UnpackUDP(respBuf[:rn])
	if err != nil {
		t.Fatalf("unpack udp failed: %v", err)
	}
	if string(respPayload) != string(payload) {
		t.Errorf("expected %s, got %s", payload, respPayload)
	}
}

func TestHTTPProxy_ConnectAndStandard(t *testing.T) {
	// 1. Direct HTTP echo server
	ts := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Test", "Passed")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("Hello from HTTP backend"))
		}),
	}
	httpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("http listen failed: %v", err)
	}
	defer httpL.Close()
	go func() {
		_ = ts.Serve(httpL)
	}()

	r := router.NewRouter()
	defer r.Close()

	// 2. Start HTTP proxy server with auth
	httpProxy := NewHTTPServer(HTTPConfig{
		ListenAddr: "127.0.0.1:0",
		Username:   "user",
		Password:   "pass",
		Router:     r,
	})
	if err := httpProxy.Start(); err != nil {
		t.Fatalf("http proxy start failed: %v", err)
	}
	defer httpProxy.Stop()

	proxyAddr := httpProxy.Addr().String()

	// 3. Test HTTP GET with missing auth -> should receive 407
	c1, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy failed: %v", err)
	}
	reqNoAuth := fmt.Sprintf("GET http://%s/test HTTP/1.1\r\nHost: %s\r\n\r\n", httpL.Addr(), httpL.Addr())
	_, _ = c1.Write([]byte(reqNoAuth))
	br := bufio.NewReader(c1)
	respLine, _ := br.ReadString('\n')
	c1.Close()
	if !bytes.Contains([]byte(respLine), []byte("407")) {
		t.Errorf("expected 407 without auth, got %s", respLine)
	}

	// 4. Test HTTP GET with valid Basic Auth
	c2, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy failed: %v", err)
	}
	defer c2.Close()

	authVal := base64.StdEncoding.EncodeToString([]byte("user:pass"))
	reqWithAuth := fmt.Sprintf("GET http://%s/test HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n",
		httpL.Addr(), httpL.Addr(), authVal)
	_, _ = c2.Write([]byte(reqWithAuth))
	br2 := bufio.NewReader(c2)
	httpResp, err := http.ReadResponse(br2, nil)
	if err != nil {
		t.Fatalf("read response failed: %v", err)
	}
	body, _ := io.ReadAll(httpResp.Body)
	httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", httpResp.StatusCode)
	}
	if string(body) != "Hello from HTTP backend" {
		t.Errorf("expected 'Hello from HTTP backend', got %s", body)
	}

	// 5. Test CONNECT method with auth
	c3, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy failed: %v", err)
	}
	defer c3.Close()

	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n",
		httpL.Addr(), httpL.Addr(), authVal)
	_, _ = c3.Write([]byte(connectReq))
	br3 := bufio.NewReader(c3)
	connectRespLine, err := br3.ReadString('\n')
	if err != nil {
		t.Fatalf("read connect reply failed: %v", err)
	}
	if !bytes.Contains([]byte(connectRespLine), []byte("200 Connection Established")) {
		t.Errorf("expected 200 Connection Established, got %s", connectRespLine)
	}
}

func skipWithoutIPv6Loopback(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	_ = ln.Close()
}

func TestSocks5IPv6TCPAndUDP(t *testing.T) {
	skipWithoutIPv6Loopback(t)

	echoL, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoL.Close()
	go func() {
		for {
			conn, err := echoL.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	udpEcho, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer udpEcho.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, rAddr, err := udpEcho.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = udpEcho.WriteToUDP(buf[:n], rAddr)
		}
	}()

	r := router.NewRouter()
	defer r.Close()
	s5 := NewSocks5Server(Socks5Config{ListenAddr: "[::1]:0", Router: r})
	if err := s5.Start(); err != nil {
		t.Fatal(err)
	}
	defer s5.Stop()

	tcpAddr, err := net.ResolveTCPAddr("tcp", s5.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := network.Sock5Handshake(client, 5000, "", ""); err != nil {
		t.Fatal(err)
	}
	echoHost, echoPortStr, _ := net.SplitHostPort(echoL.Addr().String())
	echoPort, _ := strconv.Atoi(echoPortStr)
	if err := network.Sock5SetRequest(client, echoHost, echoPort, 5000); err != nil {
		t.Fatalf("ipv6 connect: %v", err)
	}
	msg := []byte("ipv6 hello")
	if _, err := client.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(msg) {
		t.Fatalf("tcp echo %q", got)
	}

	udpClient, err := net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer udpClient.Close()
	if err := network.Sock5Handshake(udpClient, 5000, "", ""); err != nil {
		t.Fatal(err)
	}
	relayAddr, err := network.Sock5SetUDPRequest(udpClient, "::", 0, 5000)
	if err != nil {
		t.Fatalf("udp associate: %v", err)
	}
	relay, err := net.ResolveUDPAddr("udp", relayAddr)
	if err != nil {
		t.Fatal(err)
	}
	if relay.IP.To4() != nil {
		t.Fatalf("UDP relay should be IPv6, got %s", relayAddr)
	}
	clientUDP, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback, Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer clientUDP.Close()
	udpHost, udpPortStr, _ := net.SplitHostPort(udpEcho.LocalAddr().String())
	udpPort, _ := strconv.Atoi(udpPortStr)
	payload := []byte("ipv6 udp")
	pkt, err := network.Sock5PackUDP(udpHost, udpPort, payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientUDP.WriteToUDP(pkt, relay); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	_ = clientUDP.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _, err := clientUDP.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	host, port, data, err := network.Sock5UnpackUDP(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(payload) || port != udpPort {
		t.Fatalf("udp reply %s:%d %q", host, port, data)
	}
}

func TestSocks5IPv6RejectionOnProxy(t *testing.T) {
	mockUpstreamAddr, stopMock := startMockUpstreamSocks5(t)
	defer stopMock()

	// Default router: all unknown non-China destinations route to Proxy (SPP)
	r := router.NewRouter()
	defer r.Close()

	s5 := NewSocks5Server(Socks5Config{
		ListenAddr: "127.0.0.1:0",
		Router:     r,
		Upstream:   &mockUpstream{addr: mockUpstreamAddr},
	})
	if err := s5.Start(); err != nil {
		t.Fatal(err)
	}
	defer s5.Stop()

	tcpAddr, err := net.ResolveTCPAddr("tcp", s5.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if err := network.Sock5Handshake(client, 5000, "", ""); err != nil {
		t.Fatal(err)
	}

	// Try connecting to an IPv6 literal (Google IPv6) which routes to Proxy
	err = network.Sock5SetRequest(client, "2607:f8b0:400a:801::200e", 443, 5000)
	if err == nil {
		t.Fatalf("expected error when connecting to IPv6 literal via SPP proxy, got success")
	}
}

func TestHTTPProxyIPv6Rejection(t *testing.T) {
	mockUpstreamAddr, stopMock := startMockUpstreamSocks5(t)
	defer stopMock()

	r := router.NewRouter()
	defer r.Close()

	httpSrv := NewHTTPServer(HTTPConfig{
		ListenAddr: "127.0.0.1:0",
		Router:     r,
		Upstream:   &mockUpstream{addr: mockUpstreamAddr},
	})
	if err := httpSrv.Start(); err != nil {
		t.Fatal(err)
	}
	defer httpSrv.Stop()

	client, err := net.Dial("tcp", httpSrv.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// CONNECT to IPv6 literal
	req := "CONNECT [2607:f8b0:400a:801::200e]:443 HTTP/1.1\r\nHost: [2607:f8b0:400a:801::200e]:443\r\n\r\n"
	if _, err := client.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}

	respBuf := make([]byte, 1024)
	n, err := client.Read(respBuf)
	if err != nil {
		t.Fatal(err)
	}
	resp := string(respBuf[:n])
	if !bytes.Contains([]byte(resp), []byte("502 Bad Gateway")) {
		t.Fatalf("expected 502 Bad Gateway, got %q", resp)
	}
}

// mustTCPAddr resolves an address or fails the test.
func mustTCPAddr(t *testing.T, addr string) *net.TCPAddr {
	t.Helper()
	a, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		t.Fatalf("resolve %s: %v", addr, err)
	}
	return a
}

// startEchoTCP starts a loopback TCP server that echoes every byte back.
func startEchoTCP(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// A fully idle proxied connection must be reclaimed by the idle timeout
// instead of leaking its goroutines and sockets forever.
func TestSocks5IdleTimeoutReclaimsConnection(t *testing.T) {
	echoAddr, stopEcho := startEchoTCP(t)
	defer stopEcho()

	r := router.NewRouter()
	defer r.Close()
	s5 := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0", Router: r, IdleTimeout: 200 * time.Millisecond})
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
	host, portStr, _ := net.SplitHostPort(echoAddr)
	port, _ := strconv.Atoi(portStr)
	if err := network.Sock5SetRequest(cc, host, port, 5000); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Send nothing after the tunnel opens; the idle deadline must close it.
	_ = cc.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := cc.Read(make([]byte, 4)); err == nil {
		t.Fatal("expected the idle connection to be closed, but read succeeded")
	}
}

// Active traffic must keep refreshing the idle deadline (long-lived transfer).
func TestSocks5ActiveTrafficSurvivesIdleTimeout(t *testing.T) {
	echoAddr, stopEcho := startEchoTCP(t)
	defer stopEcho()

	r := router.NewRouter()
	defer r.Close()
	s5 := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0", Router: r, IdleTimeout: 200 * time.Millisecond})
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
	host, portStr, _ := net.SplitHostPort(echoAddr)
	port, _ := strconv.Atoi(portStr)
	if err := network.Sock5SetRequest(cc, host, port, 5000); err != nil {
		t.Fatalf("connect: %v", err)
	}

	msg := []byte("ping")
	reply := make([]byte, len(msg))
	for i := 0; i < 4; i++ {
		if _, err := cc.Write(msg); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if _, err := io.ReadFull(cc, reply); err != nil {
			t.Fatalf("echo %d failed while traffic was flowing: %v", i, err)
		}
		time.Sleep(120 * time.Millisecond) // cross the idle threshold several times
	}
}

// A one-way transfer (server streaming data, client sending nothing) must not
// be killed by the idle timeout: activity in either direction keeps it alive.
func TestSocks5OneWayStreamSurvivesIdleTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			ticker := time.NewTicker(30 * time.Millisecond)
			defer ticker.Stop()
			for i := 0; i < 10; i++ {
				if _, err := c.Write([]byte("DATA\n")); err != nil {
					return
				}
				<-ticker.C
			}
		}(c)
	}()

	r := router.NewRouter()
	defer r.Close()
	s5 := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0", Router: r, IdleTimeout: 500 * time.Millisecond})
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
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	if err := network.Sock5SetRequest(cc, host, port, 5000); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Client only reads; it never sends anything over the tunnel. The 300ms of
	// continuous downstream data spans multiple 150ms idle windows.
	_ = cc.SetReadDeadline(time.Now().Add(3 * time.Second))
	br := bufio.NewReader(cc)
	for i := 0; i < 10; i++ {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("one-way stream cut off at chunk %d: %v", i, err)
		}
		if line != "DATA\n" {
			t.Fatalf("unexpected chunk %q", line)
		}
	}
}

// Bytes a client sends coalesced with the CONNECT request (already sitting in
// the proxy's bufio.Reader) must still reach the target instead of being lost.
func TestHTTPConnectPreservesCoalescedTunnelBytes(t *testing.T) {
	echoAddr, stopEcho := startEchoTCP(t)
	defer stopEcho()

	r := router.NewRouter()
	defer r.Close()
	hp := NewHTTPServer(HTTPConfig{ListenAddr: "127.0.0.1:0", Router: r})
	if err := hp.Start(); err != nil {
		t.Fatal(err)
	}
	defer hp.Stop()

	cc, err := net.Dial("tcp", hp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	payload := []byte("EARLY-TUNNEL-BYTES")
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	// One write coalesces the request line/headers and the first tunnel frame.
	if _, err := cc.Write(append([]byte(req), payload...)); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(cc)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read connect reply: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	_ = cc.SetReadDeadline(time.Now().Add(3 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Fatalf("coalesced tunnel bytes were not forwarded: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("expected %q, got %q", payload, got)
	}
}

// After a UDP association is pinned to the first client, datagrams from any
// other source must be dropped (RFC 1928).
func TestSocks5UDPRejectsSpoofedSource(t *testing.T) {
	udpEcho, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer udpEcho.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, rAddr, err := udpEcho.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = udpEcho.WriteToUDP(buf[:n], rAddr)
		}
	}()

	r := router.NewRouter()
	defer r.Close()
	s5 := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0", Router: r})
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
	relayAddrStr, err := network.Sock5SetUDPRequest(cc, "0.0.0.0", 0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	relayUDP, _ := net.ResolveUDPAddr("udp", relayAddrStr)

	echoHost, echoPortStr, _ := net.SplitHostPort(udpEcho.LocalAddr().String())
	echoPort, _ := strconv.Atoi(echoPortStr)

	// Legitimate client pins its endpoint and gets an echo.
	legit, err := net.ListenUDP("udp", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer legit.Close()
	good, _ := network.Sock5PackUDP(echoHost, echoPort, []byte("legit"))
	if _, err := legit.WriteToUDP(good, relayUDP); err != nil {
		t.Fatal(err)
	}
	_ = legit.SetReadDeadline(time.Now().Add(3 * time.Second))
	rbuf := make([]byte, 65535)
	if _, _, err := legit.ReadFromUDP(rbuf); err != nil {
		t.Fatalf("legit echo: %v", err)
	}

	// Attacker on a different socket tries to inject; the relay must drop it.
	attacker, err := net.ListenUDP("udp", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer attacker.Close()
	evil, _ := network.Sock5PackUDP(echoHost, echoPort, []byte("spoofed"))
	if _, err := attacker.WriteToUDP(evil, relayUDP); err != nil {
		t.Fatal(err)
	}

	// The legit client must not receive the attacker's echoed payload; after
	// the single legit echo above the relay should stay silent.
	_ = legit.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if n, _, err := legit.ReadFromUDP(rbuf); err == nil {
		t.Fatalf("relay accepted a spoofed datagram and echoed %d bytes", n)
	}
}

// The direct outbound socket is unconnected: a datagram arriving at its
// ephemeral port from an address other than the dialed target must not be
// relayed back to the SOCKS client (a local process or off-path sender could
// otherwise inject a forged answer, e.g. a fake DNS reply).
func TestSocks5UDPRejectsSpoofedDirectUpstream(t *testing.T) {
	sawProxy := make(chan *net.UDPAddr, 4)
	udpEcho, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer udpEcho.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, rAddr, err := udpEcho.ReadFromUDP(buf)
			if err != nil {
				return
			}
			select {
			case sawProxy <- rAddr:
			default:
			}
			_, _ = udpEcho.WriteToUDP(buf[:n], rAddr)
		}
	}()

	r := router.NewRouter()
	defer r.Close()
	s5 := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0", Router: r})
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
	relayAddrStr, err := network.Sock5SetUDPRequest(cc, "0.0.0.0", 0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	relayUDP, _ := net.ResolveUDPAddr("udp", relayAddrStr)

	echoHost, echoPortStr, _ := net.SplitHostPort(udpEcho.LocalAddr().String())
	echoPort, _ := strconv.Atoi(echoPortStr)

	client, err := net.ListenUDP("udp", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	good, _ := network.Sock5PackUDP(echoHost, echoPort, []byte("legit"))
	if _, err := client.WriteToUDP(good, relayUDP); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	rbuf := make([]byte, 65535)
	if _, _, err := client.ReadFromUDP(rbuf); err != nil {
		t.Fatalf("legit echo: %v", err)
	}

	// The echo server saw the proxy's per-target outbound socket; an attacker
	// that learns that endpoint fires a raw datagram at it from another addr.
	var proxySession *net.UDPAddr
	select {
	case proxySession = <-sawProxy:
	case <-time.After(2 * time.Second):
		t.Fatal("never observed proxy outbound socket")
	}
	attacker, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer attacker.Close()
	if _, err := attacker.WriteToUDP([]byte("spoofed raw upstream answer"), proxySession); err != nil {
		t.Fatal(err)
	}

	_ = client.SetReadDeadline(time.Now().Add(600 * time.Millisecond))
	if n, _, err := client.ReadFromUDP(rbuf); err == nil {
		t.Fatalf("relay forwarded a spoofed upstream datagram (%d bytes)", n)
	}
}

// A domain-name (ATYP 0x03) direct target must be resolved once: the reply
// filter and the outbound write share the resolved endpoint, so a round-robin
// name cannot make the loop pin a different peer than the datagrams hit.
func TestSocks5UDPDomainTargetSharesResolvedEndpoint(t *testing.T) {
	udpEcho, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer udpEcho.Close()
	echoPort := udpEcho.LocalAddr().(*net.UDPAddr).Port
	go func() {
		buf := make([]byte, 2048)
		for {
			n, rAddr, err := udpEcho.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = udpEcho.WriteToUDP(buf[:n], rAddr)
		}
	}()

	// The test is only meaningful where "localhost" resolves to the v4 echo.
	resolved, err := net.ResolveUDPAddr("udp", net.JoinHostPort("localhost", strconv.Itoa(echoPort)))
	if err != nil || resolved.IP.To4() == nil {
		t.Skipf("localhost does not resolve to an IPv4 address: %v", err)
	}

	r := router.NewRouter()
	defer r.Close()
	r.AddDirectDomain("localhost")
	s5 := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0", Router: r})
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
	relayAddrStr, err := network.Sock5SetUDPRequest(cc, "0.0.0.0", 0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	relayUDP, _ := net.ResolveUDPAddr("udp", relayAddrStr)

	client, err := net.ListenUDP("udp", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	payload := []byte("domain target")
	pkt, _ := network.Sock5PackUDP("localhost", echoPort, payload)
	if _, err := client.WriteToUDP(pkt, relayUDP); err != nil {
		t.Fatal(err)
	}

	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	rbuf := make([]byte, 65535)
	n, _, err := client.ReadFromUDP(rbuf)
	if err != nil {
		t.Fatalf("reply for domain target was dropped: %v", err)
	}
	srcHost, srcPort, data, err := network.Sock5UnpackUDP(rbuf[:n])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(payload) || srcPort != echoPort || net.ParseIP(srcHost) == nil {
		t.Fatalf("unexpected reply from %s:%d (%q)", srcHost, srcPort, data)
	}
}

// The SPP-side UDP socket is likewise unconnected: only the relay endpoint
// advertised in the UDP ASSOCIATE reply may deliver upstream datagrams.
func TestSocks5UDPRejectsSpoofedSPPRelay(t *testing.T) {
	gotClientSrc := make(chan *net.UDPAddr, 4)
	tcpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpL.Close()

	go func() {
		c, err := tcpL.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = network.Sock5HandshakeBy(c, "", "")
		var head [4]byte
		if _, err := io.ReadFull(c, head[:]); err != nil {
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
		if err := network.Sock5SendConnectReply(c, 0x00, net.JoinHostPort("127.0.0.1", strconv.Itoa(relayPort))); err != nil {
			return
		}
		go func() {
			buf := make([]byte, 65535)
			for {
				_, src, err := uRelay.ReadFromUDP(buf)
				if err != nil {
					return
				}
				select {
				case gotClientSrc <- src:
				default:
				}
				// Intentionally never answer: any packet the SOCKS client
				// receives can only come from a spoofing sender.
			}
		}()
		dummy := make([]byte, 1)
		_, _ = c.Read(dummy)
	}()

	r := router.NewRouter()
	defer r.Close()
	s5 := NewSocks5Server(Socks5Config{
		ListenAddr: "127.0.0.1:0",
		Router:     r,
		Upstream:   &mockUpstream{addr: tcpL.Addr().String()},
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
	relayAddrStr, err := network.Sock5SetUDPRequest(cc, "0.0.0.0", 0, 5000)
	if err != nil {
		t.Fatal(err)
	}
	relayUDP, _ := net.ResolveUDPAddr("udp", relayAddrStr)

	// 8.8.8.8 is outside reserved CIDRs, so the route is Proxy via the mock SPP.
	client, err := net.ListenUDP("udp", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	pkt, _ := network.Sock5PackUDP("8.8.8.8", 53, []byte("legit dns query"))
	if _, err := client.WriteToUDP(pkt, relayUDP); err != nil {
		t.Fatal(err)
	}

	var proxySppSocket *net.UDPAddr
	select {
	case proxySppSocket = <-gotClientSrc:
	case <-time.After(3 * time.Second):
		t.Fatal("mock SPP relay never saw the proxy's UDP session")
	}

	// A sender other than the advertised relay fires a fully framed answer at
	// the proxy's SPP-side socket.
	attacker, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer attacker.Close()
	evil, _ := network.Sock5PackUDP("8.8.8.8", 53, []byte("spoofed dns answer"))
	if _, err := attacker.WriteToUDP(evil, proxySppSocket); err != nil {
		t.Fatal(err)
	}

	_ = client.SetReadDeadline(time.Now().Add(600 * time.Millisecond))
	rbuf := make([]byte, 65535)
	if n, _, err := client.ReadFromUDP(rbuf); err == nil {
		t.Fatalf("relay forwarded a datagram from a non-relay SPP sender (%d bytes)", n)
	}
}

// startRawHTTPBackend accepts one connection, captures the full request head
// block and replies 200. Unlike net/http.Server it applies no Host fallback,
// exposing exactly what the proxy put on the wire.
func startRawHTTPBackend(t *testing.T) (string, func() string) {
	t.Helper()
	bl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rawCh := make(chan string, 1)
	go func() {
		conn, err := bl.Accept()
		if err != nil {
			rawCh <- ""
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		var sb strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				break
			}
			sb.WriteString(line)
			if line == "\r\n" || line == "\n" {
				break
			}
		}
		rawCh <- sb.String()
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
	}()
	return bl.Addr().String(), func() string {
		select {
		case raw := <-rawCh:
			return raw
		case <-time.After(3 * time.Second):
			t.Fatal("backend never received a request")
			return ""
		}
	}
}

// RFC 7230 5.4: a proxy MUST forward the client Host header. The request line
// is rewritten to origin form, but Host is preserved verbatim so name-based
// backends (nginx vhosts, CDNs) do not answer 400.
func TestHTTPProxyForwardsHostHeader(t *testing.T) {
	r := router.NewRouter()
	defer r.Close()
	hp := NewHTTPServer(HTTPConfig{ListenAddr: "127.0.0.1:0", Router: r})
	if err := hp.Start(); err != nil {
		t.Fatal(err)
	}
	defer hp.Stop()

	// 1. HTTP/1.1 with a virtual-host Host that differs from the socket peer.
	backend, getRaw := startRawHTTPBackend(t)
	cc, err := net.Dial("tcp", hp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	req := fmt.Sprintf("GET http://%s/path HTTP/1.1\r\nHost: vhost.example\r\nX-Trailer: ok\r\n\r\n", backend)
	if _, err := cc.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	if _, err := http.ReadResponse(bufio.NewReader(cc), nil); err != nil {
		t.Fatalf("response: %v", err)
	}
	_ = cc.Close()

	raw := getRaw()
	if !strings.HasPrefix(raw, "GET /path HTTP/1.1\r\n") {
		t.Errorf("request line not rewritten to origin form:\n%s", raw)
	}
	if !strings.Contains(raw, "Host: vhost.example\r\n") {
		t.Errorf("client Host header lost in forwarding:\n%s", raw)
	}
	if strings.Contains(raw, "\nHost:") && strings.Count(raw, "Host:") != 1 {
		t.Errorf("Host header duplicated:\n%s", raw)
	}

	// 2. HTTP/1.0 without Host: the proxy synthesizes one from the target.
	backend2, getRaw2 := startRawHTTPBackend(t)
	cc2, err := net.Dial("tcp", hp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cc2.Write([]byte(fmt.Sprintf("GET http://%s/ HTTP/1.0\r\n\r\n", backend2))); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(cc2)
	if _, err := http.ReadResponse(br, nil); err != nil {
		t.Fatalf("response: %v", err)
	}
	_ = cc2.Close()

	raw2 := getRaw2()
	// Backend uses a non-default (ephemeral) port, so the synthesized Host
	// keeps it.
	want := "Host: " + backend2 + "\r\n"
	if !strings.Contains(raw2, want) {
		t.Errorf("missing synthesized Host %q, got:\n%s", want, raw2)
	}
}

// The hand-rolled header parser used to place no bound on the request line
// or the header block: a client sending a large request quickly (the 15s
// idle deadline only stops slow sends) could grow proxy memory without
// limit. The head is now capped at 1 MiB and rejected with 400 before the
// proxy contacts any upstream.
func TestHTTPProxyRejectsOversizedHead(t *testing.T) {
	r := router.NewRouter()
	defer r.Close()
	hp := NewHTTPServer(HTTPConfig{ListenAddr: "127.0.0.1:0", Router: r})
	if err := hp.Start(); err != nil {
		t.Fatal(err)
	}
	defer hp.Stop()

	// Canary upstream: the proxy must never dial it while rejecting.
	cl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	var contacted int32
	go func() {
		for {
			c, err := cl.Accept()
			if err != nil {
				return
			}
			atomic.StoreInt32(&contacted, 1)
			_ = c.Close()
		}
	}()
	target := cl.Addr().String()

	// The proxy is allowed to reset the connection while the client is still
	// streaming the oversized head, so write errors are tolerated. When a
	// response does arrive it must be 400. The hard contract is that no
	// upstream connection is ever made.
	sendHead := func(t *testing.T, head string) {
		t.Helper()
		cc, err := net.Dial("tcp", hp.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer cc.Close()
		go func() { _, _ = cc.Write([]byte(head)) }()
		_ = cc.SetReadDeadline(time.Now().Add(5 * time.Second))
		if resp, err := http.ReadResponse(bufio.NewReader(cc), nil); err == nil {
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		}
	}

	t.Run("oversized request line", func(t *testing.T) {
		head := "GET http://" + target + "/" + strings.Repeat("a", 2<<20) + " HTTP/1.1\r\nHost: x\r\n\r\n"
		sendHead(t, head)
	})

	t.Run("oversized header block", func(t *testing.T) {
		head := "GET http://" + target + "/ HTTP/1.1\r\n" +
			"X-Big: " + strings.Repeat("b", 2<<20) + "\r\n\r\n"
		sendHead(t, head)
	})

	// Give the proxy a moment in case it would (wrongly) dial after replying.
	time.Sleep(100 * time.Millisecond)
	if atomic.LoadInt32(&contacted) != 0 {
		t.Fatal("proxy dialed upstream while rejecting an oversized head")
	}
}

// A request whose head stays just under the 1 MiB cap must still be proxied.
func TestHTTPProxyAcceptsHeadNearLimit(t *testing.T) {
	r := router.NewRouter()
	defer r.Close()
	hp := NewHTTPServer(HTTPConfig{ListenAddr: "127.0.0.1:0", Router: r})
	if err := hp.Start(); err != nil {
		t.Fatal(err)
	}
	defer hp.Stop()

	backend, getRaw := startRawHTTPBackend(t)
	cc, err := net.Dial("tcp", hp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	value := strings.Repeat("c", (1<<20)-8192)
	head := "GET http://" + backend + "/ HTTP/1.1\r\nX-Big: " + value + "\r\n\r\n"
	if _, err := cc.Write([]byte(head)); err != nil {
		t.Fatal(err)
	}
	_ = cc.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(cc), nil)
	if err != nil {
		t.Fatalf("reading proxy response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if raw := getRaw(); !strings.Contains(raw, "X-Big: "+value) {
		t.Fatal("near-limit header was not forwarded")
	}
}

// startCountingHTTPBackend answers every request with 204 and counts the
// connections it saw, so a test can prove that rejected heads never reached
// an upstream while a well-formed control head did.
func startCountingHTTPBackend(t *testing.T) (string, *int32) {
	t.Helper()
	bl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var hits int32
	go func() {
		for {
			conn, err := bl.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&hits, 1)
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if line == "\r\n" || line == "\n" {
						break
					}
				}
				_, _ = c.Write([]byte("HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n"))
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = bl.Close() })
	return bl.Addr().String(), &hits
}

// RFC 7230 3.1.1: the request line is exactly three SP-separated printable
// tokens. A doubled-space line parses into empty method/target/version
// tokens, and the proxy used to forward that corrupted line to the origin; a
// bare CR inside a header line is forwarded as a second line and can inject
// a header there. Both must be answered with 400 and never reach an upstream.
func TestHTTPProxyRejectsMalformedRequestLine(t *testing.T) {
	r := router.NewRouter()
	defer r.Close()
	hp := NewHTTPServer(HTTPConfig{ListenAddr: "127.0.0.1:0", Router: r})
	if err := hp.Start(); err != nil {
		t.Fatal(err)
	}
	defer hp.Stop()

	backend, hits := startCountingHTTPBackend(t)
	backendHost, _, _ := net.SplitHostPort(backend)

	sendHead := func(t *testing.T, head string) {
		t.Helper()
		cc, err := net.Dial("tcp", hp.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer cc.Close()
		if _, err := cc.Write([]byte(head)); err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = cc.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(cc), nil)
		if err != nil {
			t.Fatalf("no response: %v", err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	}

	// Control: the same head with exactly one space per token must still be
	// proxied, proving the rejection below is about the malformed lines.
	t.Run("well-formed head still proxied", func(t *testing.T) {
		cc, err := net.Dial("tcp", hp.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer cc.Close()
		req := fmt.Sprintf("GET http://%s/ HTTP/1.1\r\nHost: %s\r\n\r\n", backend, backendHost)
		if _, err := cc.Write([]byte(req)); err != nil {
			t.Fatal(err)
		}
		_ = cc.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(cc), nil)
		if err != nil {
			t.Fatalf("response: %v", err)
		}
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", resp.StatusCode)
		}
	})
	atomic.StoreInt32(hits, 0) // only the malformed cases must stay at zero

	t.Run("doubled spaces in request line", func(t *testing.T) {
		sendHead(t, "GET  http://"+backend+"/  HTTP/1.1\r\nHost: "+backendHost+"\r\n\r\n")
	})
	t.Run("trailing space in request line", func(t *testing.T) {
		sendHead(t, "GET http://"+backend+"/ HTTP/1.1 \r\nHost: "+backendHost+"\r\n\r\n")
	})
	t.Run("control octet in request target", func(t *testing.T) {
		sendHead(t, "GET http://"+backend+"/\x00 HTTP/1.1\r\nHost: "+backendHost+"\r\n\r\n")
	})
	t.Run("non-http version token", func(t *testing.T) {
		sendHead(t, "GET http://"+backend+"/ FTP/1.1\r\nHost: "+backendHost+"\r\n\r\n")
	})
	t.Run("bare cr inside header line", func(t *testing.T) {
		sendHead(t, "GET http://"+backend+"/ HTTP/1.1\r\nHost: "+backendHost+"\rX-Injected: 1\r\n\r\n")
	})

	time.Sleep(100 * time.Millisecond)
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("malformed heads reached the upstream %d times", got)
	}
}

// A CONNECT or absolute-form target whose port token is not a number in
// 1..65535 is a malformed request: answering 400 is correct, and dialing
// "host:99999" just to fail is not.
func TestHTTPProxyRejectsOutOfRangePort(t *testing.T) {
	r := router.NewRouter()
	defer r.Close()
	hp := NewHTTPServer(HTTPConfig{ListenAddr: "127.0.0.1:0", Router: r})
	if err := hp.Start(); err != nil {
		t.Fatal(err)
	}
	defer hp.Stop()

	sendHead := func(t *testing.T, head string) {
		t.Helper()
		cc, err := net.Dial("tcp", hp.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer cc.Close()
		if _, err := cc.Write([]byte(head)); err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = cc.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(cc), nil)
		if err != nil {
			t.Fatalf("no response: %v", err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	}

	t.Run("connect port above 65535", func(t *testing.T) {
		sendHead(t, "CONNECT example.com:99999 HTTP/1.1\r\nHost: example.com:99999\r\n\r\n")
	})
	t.Run("absolute uri port zero", func(t *testing.T) {
		sendHead(t, "GET http://example.com:0/ HTTP/1.1\r\nHost: example.com:0\r\n\r\n")
	})
	t.Run("absolute uri negative port", func(t *testing.T) {
		sendHead(t, "GET http://example.com:-1/ HTTP/1.1\r\nHost: example.com:-1\r\n\r\n")
	})
	t.Run("host header non-numeric port", func(t *testing.T) {
		sendHead(t, "GET / HTTP/1.1\r\nHost: example.com:abc\r\n\r\n")
	})
}

// A peer that keeps sending but never reads must not pin the relay: the busy
// direction refreshes the idle watchdog on every successful read, so a
// stalled write used to block until the TCP retransmit timeout. The write
// deadline bounds it by the idle window instead.
func TestRelayWriteDeadlineBoundsStalledPeer(t *testing.T) {
	bl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer bl.Close()

	backendDone := make(chan struct{})
	t.Cleanup(func() { close(backendDone) })
	go func() {
		c, err := bl.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 1024)
		for {
			// Keep sending so the relay's read side stays busy and the idle
			// watchdog never fires...
			if _, err := c.Write(buf); err != nil {
				return
			}
			select {
			case <-backendDone:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()

	r := router.NewRouter()
	defer r.Close()
	s5 := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0", Router: r, IdleTimeout: 300 * time.Millisecond})
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
	host, portStr, _ := net.SplitHostPort(bl.Addr().String())
	port, _ := strconv.Atoi(portStr)
	if err := network.Sock5SetRequest(cc, host, port, 5000); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// The client floods the tunnel; the backend never reads, so the relay's
	// writes to it stall once the socket buffers fill.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := cc.Write(buf); err != nil {
				return
			}
		}
	}()

	// Drain the backend's trickle so the opposite pipe keeps succeeding and
	// the watchdog stays quiet: only the write deadline can end this relay.
	// A close after the idle window (well under the guard) is the fix; one
	// that only happens at the guard deadline is the old unbounded stall.
	start := time.Now()
	_ = cc.SetReadDeadline(start.Add(10 * time.Second))
	for {
		if _, err := cc.Read(make([]byte, 4096)); err != nil {
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("relay held a stalled write open for %v, past the idle window", elapsed)
			}
			return
		}
	}
}

type trafficRecordingUpstream struct {
	addr         string
	uploadN      int64
	downloadN    int64
	lastWireNano int64
}

func (u *trafficRecordingUpstream) Socks5Addr() string          { return u.addr }
func (u *trafficRecordingUpstream) Socks5Auth() (string, string) { return "", "" }
func (u *trafficRecordingUpstream) PickNode() (string, string, func()) {
	return u.addr, "narrow-pipe-node", func() {}
}
func (u *trafficRecordingUpstream) RecordNodeTraffic(_ string, up, down int64) {
	atomic.AddInt64(&u.uploadN, up)
	atomic.AddInt64(&u.downloadN, down)
	atomic.StoreInt64(&u.lastWireNano, time.Now().UnixNano())
}
func (u *trafficRecordingUpstream) HasRecentNodeTraffic(_ string, window time.Duration) bool {
	last := atomic.LoadInt64(&u.lastWireNano)
	if last <= 0 {
		return false
	}
	return time.Since(time.Unix(0, last)) <= window
}

// startMinimalSocks5Tunnel simulates an SPP local SOCKS5 listener that forwards
// TCP streams to the requested target address and mimics SPP's behavior where
// an EOF from the client closes the tunnel once the upload has drained.
func startMinimalSocks5Tunnel(t *testing.T) string {
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
			go func(clientConn net.Conn) {
				defer clientConn.Close()
				if err := network.Sock5HandshakeBy(clientConn, "", ""); err != nil {
					return
				}
				cmd, _, targetAddr, err := network.Sock5GetRequest(clientConn)
				if err != nil || cmd != network.Socks5CmdConnect {
					return
				}
				if err := network.Sock5SendConnectReply(clientConn, 0, "0.0.0.0:0"); err != nil {
					return
				}
				targetConn, err := net.DialTimeout("tcp", targetAddr, 5*time.Second)
				if err != nil {
					return
				}
				defer targetConn.Close()

				done := make(chan struct{}, 2)
				go func() {
					_, _ = io.Copy(targetConn, clientConn)
					if cw, ok := targetConn.(interface{ CloseWrite() error }); ok {
						_ = cw.CloseWrite()
					}
					done <- struct{}{}
				}()
				go func() {
					_, _ = io.Copy(clientConn, targetConn)
					if cw, ok := clientConn.(interface{ CloseWrite() error }); ok {
						_ = cw.CloseWrite()
					}
					done <- struct{}{}
				}()
				<-done
				<-done
			}(c)
		}
	}()
	return ln.Addr().String()
}

func startNarrowPipeStreamServer(t *testing.T, uploadRateBps int64, onWireProgress func()) string {
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
			go func(conn net.Conn) {
				defer conn.Close()
				var hdr [9]byte
				if _, err := io.ReadFull(conn, hdr[:]); err != nil {
					return
				}
				mode := hdr[0]
				total := int64(uint64(hdr[1])<<56 | uint64(hdr[2])<<48 | uint64(hdr[3])<<40 | uint64(hdr[4])<<32 |
					uint64(hdr[5])<<24 | uint64(hdr[6])<<16 | uint64(hdr[7])<<8 | uint64(hdr[8]))
				switch mode {
				case 'U':
					h := sha256.New()
					buf := make([]byte, 64*1024)
					var received int64
					start := time.Now()
					for received < total {
						toRead := int64(len(buf))
						if rem := total - received; rem < toRead {
							toRead = rem
						}
						n, err := conn.Read(buf[:toRead])
						if n > 0 {
							_, _ = h.Write(buf[:n])
							received += int64(n)
							if onWireProgress != nil {
								onWireProgress()
							}
							if uploadRateBps > 0 {
								expected := time.Duration(float64(received) / float64(uploadRateBps) * float64(time.Second))
								if sleep := expected - time.Since(start); sleep > 0 {
									time.Sleep(sleep)
								}
							}
						}
						if err != nil {
							return
						}
					}
					_, _ = conn.Write(h.Sum(nil))
				case 'D':
					chunk := make([]byte, 64*1024)
					var sent int64
					var seq byte
					for sent < total {
						toWrite := int64(len(chunk))
						if rem := total - sent; rem < toWrite {
							toWrite = rem
						}
						for i := int64(0); i < toWrite; i++ {
							chunk[i] = seq + byte(i*31)
						}
						seq++
						if _, err := conn.Write(chunk[:toWrite]); err != nil {
							return
						}
						if onWireProgress != nil {
							onWireProgress()
						}
						sent += toWrite
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

func makeStreamHeader(mode byte, total int64) [9]byte {
	var hdr [9]byte
	hdr[0] = mode
	u := uint64(total)
	hdr[1] = byte(u >> 56)
	hdr[2] = byte(u >> 48)
	hdr[3] = byte(u >> 40)
	hdr[4] = byte(u >> 32)
	hdr[5] = byte(u >> 24)
	hdr[6] = byte(u >> 16)
	hdr[7] = byte(u >> 8)
	hdr[8] = byte(u)
	return hdr
}

// TestRelayNarrowPipeSlowUploadAndDownload verifies that under "大流量 + 小水管"
// (large burst + narrow pipe), both SOCKS5 and HTTP CONNECT relays:
//  1. Do not prematurely send FIN to SPP when the client half-closes (CloseWrite)
//     after finishing a large upload while the server is still slowly digesting it.
//  2. Refresh SetReadDeadline on every active Read so transfers taking longer than
//     the initial IdleTimeout/grace window complete smoothly as long as bytes trickle in.
func TestRelayNarrowPipeSlowUploadAndDownload(t *testing.T) {
	const (
		totalBytes        = 8 * 1024 * 1024 // 8 MiB
		narrowPipeRateBps = 2 * 1024 * 1024 // 2 MiB/s -> ~4s transfer (>2x the 1.5s IdleTimeout)
		idleWindow        = 1500 * time.Millisecond
	)

	sppTunnelAddr := startMinimalSocks5Tunnel(t)
	upstream := &trafficRecordingUpstream{addr: sppTunnelAddr}
	rawTargetAddr := startNarrowPipeStreamServer(t, narrowPipeRateBps, func() {
		atomic.StoreInt64(&upstream.lastWireNano, time.Now().UnixNano())
	})
	_, targetPortStr, _ := net.SplitHostPort(rawTargetAddr)
	targetPort, _ := strconv.Atoi(targetPortStr)
	targetAddr := net.JoinHostPort("localhost", targetPortStr)

	r := proxyRouterWithLocalhost(t)

	s5 := NewSocks5Server(Socks5Config{
		ListenAddr:  "127.0.0.1:0",
		Router:      r,
		Upstream:    upstream,
		IdleTimeout: idleWindow,
	})
	if err := s5.Start(); err != nil {
		t.Fatal(err)
	}
	defer s5.Stop()

	hp := NewHTTPServer(HTTPConfig{
		ListenAddr:  "127.0.0.1:0",
		Router:      r,
		Upstream:    upstream,
		IdleTimeout: idleWindow,
	})
	if err := hp.Start(); err != nil {
		t.Fatal(err)
	}
	defer hp.Stop()

	dialViaSocks5 := func(t *testing.T) *net.TCPConn {
		t.Helper()
		cc, err := net.DialTCP("tcp", nil, mustTCPAddr(t, s5.Addr().String()))
		if err != nil {
			t.Fatal(err)
		}
		if err := network.Sock5Handshake(cc, 5000, "", ""); err != nil {
			_ = cc.Close()
			t.Fatal(err)
		}
		if err := network.Sock5SetRequest(cc, "localhost", targetPort, 5000); err != nil {
			_ = cc.Close()
			t.Fatal(err)
		}
		return cc
	}

	dialViaHTTPConnect := func(t *testing.T) *net.TCPConn {
		t.Helper()
		cc, err := net.DialTCP("tcp", nil, mustTCPAddr(t, hp.Addr().String()))
		if err != nil {
			t.Fatal(err)
		}
		req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", targetAddr, targetAddr)
		if _, err := cc.Write([]byte(req)); err != nil {
			_ = cc.Close()
			t.Fatal(err)
		}
		br := bufio.NewReader(cc)
		resp, err := http.ReadResponse(br, nil)
		if err != nil || resp.StatusCode != http.StatusOK {
			_ = cc.Close()
			t.Fatalf("HTTP CONNECT failed: err=%v status=%v", err, resp)
		}
		return cc
	}

	for _, proto := range []struct {
		name string
		dial func(*testing.T) *net.TCPConn
	}{
		{"SOCKS5", dialViaSocks5},
		{"HTTP_CONNECT", dialViaHTTPConnect},
	} {
		proto := proto
		t.Run(proto.name+"_UploadWithHalfClose", func(t *testing.T) {
			cc := proto.dial(t)
			defer cc.Close()
			_ = cc.SetDeadline(time.Now().Add(30 * time.Second))

			hdr := makeStreamHeader('U', totalBytes)
			if _, err := cc.Write(hdr[:]); err != nil {
				t.Fatalf("write upload header: %v", err)
			}

			wantHash := sha256.New()
			chunk := make([]byte, 64*1024)
			var sent int64
			var seq byte
			start := time.Now()
			for sent < totalBytes {
				for i := range chunk {
					chunk[i] = seq + byte(i*31)
				}
				seq++
				_, _ = wantHash.Write(chunk)
				if _, err := cc.Write(chunk); err != nil {
					t.Fatalf("upload write at %d/%d: %v", sent, totalBytes, err)
				}
				sent += int64(len(chunk))
				expected := time.Duration(float64(sent) / float64(narrowPipeRateBps) * float64(time.Second))
				if sleep := expected - time.Since(start); sleep > 0 {
					time.Sleep(sleep)
				}
			}

			// Half-close write side while the server is still slowly digesting the 8 MiB
			// over ~4 seconds (>2x the 1.5s IdleTimeout).
			if err := cc.CloseWrite(); err != nil {
				t.Fatalf("CloseWrite: %v", err)
			}

			var gotSum [32]byte
			if _, err := io.ReadFull(cc, gotSum[:]); err != nil {
				t.Fatalf("read upload sha256 response after slow narrow-pipe drain: %v", err)
			}
			var wantSum [32]byte
			copy(wantSum[:], wantHash.Sum(nil))
			if gotSum != wantSum {
				t.Fatalf("upload sha256 mismatch: got %x, want %x", gotSum, wantSum)
			}
		})

		t.Run(proto.name+"_DownloadWithHalfClose", func(t *testing.T) {
			cc := proto.dial(t)
			defer cc.Close()
			_ = cc.SetDeadline(time.Now().Add(30 * time.Second))

			hdr := makeStreamHeader('D', totalBytes)
			if _, err := cc.Write(hdr[:]); err != nil {
				t.Fatalf("write download header: %v", err)
			}
			// Half-close immediately after sending the 9-byte download request.
			// In the old code, this set a static ReadDeadline that was never refreshed
			// on Read(n > 0) and aborted any download lasting longer than grace/idle.
			if err := cc.CloseWrite(); err != nil {
				t.Fatalf("CloseWrite: %v", err)
			}

			wantHash := sha256.New()
			expectedChunk := make([]byte, 64*1024)
			for c := int64(0); c < totalBytes/(64*1024); c++ {
				for i := range expectedChunk {
					expectedChunk[i] = byte(c) + byte(i*31)
				}
				_, _ = wantHash.Write(expectedChunk)
			}

			gotHash := sha256.New()
			buf := make([]byte, 64*1024)
			var received int64
			start := time.Now()
			for received < totalBytes {
				n, err := cc.Read(buf)
				if n > 0 {
					_, _ = gotHash.Write(buf[:n])
					received += int64(n)
					expected := time.Duration(float64(received) / float64(narrowPipeRateBps) * float64(time.Second))
					if sleep := expected - time.Since(start); sleep > 0 {
						time.Sleep(sleep)
					}
				}
				if err != nil {
					if err == io.EOF && received == totalBytes {
						break
					}
					t.Fatalf("download read at %d/%d: %v", received, totalBytes, err)
				}
			}

			var gotSum, wantSum [32]byte
			copy(gotSum[:], gotHash.Sum(nil))
			copy(wantSum[:], wantHash.Sum(nil))
			if gotSum != wantSum {
				t.Fatalf("download sha256 mismatch: got %x, want %x", gotSum, wantSum)
			}
		})
	}

	if atomic.LoadInt64(&upstream.uploadN) < totalBytes*2 {
		t.Fatalf("expected upstream upload traffic >= %d, got %d", totalBytes*2, atomic.LoadInt64(&upstream.uploadN))
	}
	if atomic.LoadInt64(&upstream.downloadN) < totalBytes*2 {
		t.Fatalf("expected upstream download traffic >= %d, got %d", totalBytes*2, atomic.LoadInt64(&upstream.downloadN))
	}
}

// TestE2E_LargeStream_1GB_UploadAndDownload streams a full 1 GiB (1,073,741,824 bytes)
// upload and a full 1 GiB download through YellowSocks -> SPP tunnel with throttled
// digestion spanning multiple IdleTimeout windows, verifying byte-for-byte SHA-256 integrity.
func TestE2E_LargeStream_1GB_UploadAndDownload(t *testing.T) {
	const (
		oneGiB          = int64(1) << 30    // 1,073,741,824 bytes (1 GiB)
		throttleRateBps = 220 * 1024 * 1024 // ~220 MiB/s -> ~4.7s per 1 GiB (>2x the 2s IdleTimeout)
	)

	sppTunnelAddr := startMinimalSocks5Tunnel(t)
	upstream := &trafficRecordingUpstream{addr: sppTunnelAddr}
	rawTargetAddr := startNarrowPipeStreamServer(t, throttleRateBps, func() {
		atomic.StoreInt64(&upstream.lastWireNano, time.Now().UnixNano())
	})
	_, targetPortStr, _ := net.SplitHostPort(rawTargetAddr)
	targetPort, _ := strconv.Atoi(targetPortStr)

	r := proxyRouterWithLocalhost(t)

	s5 := NewSocks5Server(Socks5Config{
		ListenAddr:  "127.0.0.1:0",
		Router:      r,
		Upstream:    upstream,
		IdleTimeout: 2 * time.Second,
	})
	if err := s5.Start(); err != nil {
		t.Fatal(err)
	}
	defer s5.Stop()

	var patternChunks [256][]byte
	for c := 0; c < 256; c++ {
		b := make([]byte, 64*1024)
		for i := range b {
			b[i] = byte(c) + byte(i*31)
		}
		patternChunks[c] = b
	}
	wantHash := sha256.New()
	totalChunks := oneGiB / (64 * 1024)
	for i := int64(0); i < totalChunks; i++ {
		_, _ = wantHash.Write(patternChunks[byte(i)])
	}
	var wantSum [32]byte
	copy(wantSum[:], wantHash.Sum(nil))

	dial := func(t *testing.T) *net.TCPConn {
		t.Helper()
		cc, err := net.DialTCP("tcp", nil, mustTCPAddr(t, s5.Addr().String()))
		if err != nil {
			t.Fatal(err)
		}
		if err := network.Sock5Handshake(cc, 5000, "", ""); err != nil {
			_ = cc.Close()
			t.Fatal(err)
		}
		if err := network.Sock5SetRequest(cc, "localhost", targetPort, 5000); err != nil {
			_ = cc.Close()
			t.Fatal(err)
		}
		return cc
	}

	t.Run("Upload_1GiB", func(t *testing.T) {
		start := time.Now()
		cc := dial(t)
		defer cc.Close()
		_ = cc.SetDeadline(time.Now().Add(2 * time.Minute))

		hdr := makeStreamHeader('U', oneGiB)
		if _, err := cc.Write(hdr[:]); err != nil {
			t.Fatalf("write 1GiB upload header: %v", err)
		}
		for i := int64(0); i < totalChunks; i++ {
			if _, err := cc.Write(patternChunks[byte(i)]); err != nil {
				t.Fatalf("1GiB upload write at chunk %d/%d: %v", i, totalChunks, err)
			}
		}
		if err := cc.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}
		var gotSum [32]byte
		if _, err := io.ReadFull(cc, gotSum[:]); err != nil {
			t.Fatalf("read 1GiB upload sha256 response: %v", err)
		}
		if gotSum != wantSum {
			t.Fatalf("1GiB upload sha256 mismatch: got %x, want %x", gotSum, wantSum)
		}
		t.Logf("1GiB upload verified in %s", time.Since(start))
	})

	t.Run("Download_1GiB", func(t *testing.T) {
		start := time.Now()
		cc := dial(t)
		defer cc.Close()
		_ = cc.SetDeadline(time.Now().Add(2 * time.Minute))

		hdr := makeStreamHeader('D', oneGiB)
		if _, err := cc.Write(hdr[:]); err != nil {
			t.Fatalf("write 1GiB download header: %v", err)
		}
		if err := cc.CloseWrite(); err != nil {
			t.Fatalf("CloseWrite: %v", err)
		}

		gotHash := sha256.New()
		buf := make([]byte, 64*1024)
		var received int64
		for received < oneGiB {
			n, err := cc.Read(buf)
			if n > 0 {
				_, _ = gotHash.Write(buf[:n])
				received += int64(n)
				expected := time.Duration(float64(received) / float64(throttleRateBps) * float64(time.Second))
				if sleep := expected - time.Since(start); sleep > 0 {
					time.Sleep(sleep)
				}
			}
			if err != nil {
				if err == io.EOF && received == oneGiB {
					break
				}
				t.Fatalf("1GiB download read at %d/%d: %v", received, oneGiB, err)
			}
		}
		var gotSum [32]byte
		copy(gotSum[:], gotHash.Sum(nil))
		if gotSum != wantSum {
			t.Fatalf("1GiB download sha256 mismatch: got %x, want %x", gotSum, wantSum)
		}
		t.Logf("1GiB download verified in %s", time.Since(start))
	})

	if atomic.LoadInt64(&upstream.uploadN) < oneGiB {
		t.Fatalf("expected upstream upload >= 1GiB, got %d", atomic.LoadInt64(&upstream.uploadN))
	}
	if atomic.LoadInt64(&upstream.downloadN) < oneGiB {
		t.Fatalf("expected upstream download >= 1GiB, got %d", atomic.LoadInt64(&upstream.downloadN))
	}
}
