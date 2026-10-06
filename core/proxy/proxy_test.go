package proxy

import (
	"bufio"
	"bytes"
	"encoding/base64"
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
					relayStreams(c, dstConn, defaultIdleTimeout)
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
	s5 := NewSocks5Server(Socks5Config{ListenAddr: "127.0.0.1:0", Router: r, IdleTimeout: 150 * time.Millisecond})
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
	if _, err := io.ReadFull(cc, got); err != nil {
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
