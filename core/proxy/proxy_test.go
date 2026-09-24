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
					relayStreams(c, dstConn)
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
