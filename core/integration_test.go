package core

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
	"github.com/esrrhs/yellowsocks/core/sppclient"
	"github.com/miekg/dns"
)

// Start mock upstream SOCKS5 server for integration testing
func startMockUpstream(t *testing.T) (string, func()) {
	tcpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen mock upstream: %v", err)
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
				var host string
				switch atyp {
				case 0x01:
					var ip [4]byte
					_, _ = io.ReadFull(c, ip[:])
					host = net.IP(ip[:]).String()
				case 0x03:
					var l [1]byte
					_, _ = io.ReadFull(c, l[:])
					buf := make([]byte, int(l[0]))
					_, _ = io.ReadFull(c, buf)
					host = string(buf)
				case 0x04:
					var ip [16]byte
					_, _ = io.ReadFull(c, ip[:])
					host = net.IP(ip[:]).String()
				}
				var pBuf [2]byte
				_, _ = io.ReadFull(c, pBuf[:])
				port := int(pBuf[0])<<8 | int(pBuf[1])

				if cmd == network.Socks5CmdConnect {
					target := net.JoinHostPort(host, strconv.Itoa(port))
					dstConn, err := net.DialTimeout("tcp", target, 5*time.Second)
					if err != nil {
						_ = network.Sock5SendConnectReply(c, 0x05, "0.0.0.0:0")
						return
					}
					defer dstConn.Close()
					_ = network.Sock5SendConnectReply(c, 0x00, "0.0.0.0:0")
					go func() { _, _ = io.Copy(dstConn, c) }()
					_, _ = io.Copy(c, dstConn)
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
							tDst, err := net.ResolveUDPAddr("udp", net.JoinHostPort(dstH, strconv.Itoa(dstP)))
							if err != nil {
								continue
							}
							fwdConn, err := net.ListenUDP("udp", nil)
							if err != nil {
								continue
							}
							_, _ = fwdConn.WriteToUDP(payload, tDst)
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

					dummy := make([]byte, 1)
					_, _ = c.Read(dummy)
				}
			}(conn)
		}
	}()

	return tcpL.Addr().String(), func() {
		close(stopCh)
		_ = tcpL.Close()
	}
}

func TestEngineFullIntegration(t *testing.T) {
	// 1. Pick unused ports
	getFreePort := func(networkType string) string {
		if networkType == "udp" {
			l, _ := net.ListenPacket("udp", "127.0.0.1:0")
			addr := l.LocalAddr().String()
			_ = l.Close()
			return addr
		}
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		addr := l.Addr().String()
		_ = l.Close()
		return addr
	}

	dnsUDPPort := getFreePort("udp")
	dohTCPPort := getFreePort("tcp")
	socks5Port := getFreePort("tcp")
	httpPort := getFreePort("tcp")

	upstreamAddr, cleanupUpstream := startMockUpstream(t)
	defer cleanupUpstream()

	// 2. Start TCP and UDP echo targets
	tcpEchoL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp echo listen error: %v", err)
	}
	defer tcpEchoL.Close()
	go func() {
		for {
			c, err := tcpEchoL.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()

	udpEcho, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("udp echo listen error: %v", err)
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

	// 3. Configure and start Engine with DisableTun: true
	engineCfg := EngineConfig{
		DisableTun:   true,
		SPPServer:    upstreamAddr,
		SPPProto:     "tcp",
		LocalSocks5:  upstreamAddr, // use mock upstream directly for SPP socks5
		SPPNodes: []*sppclient.Node{
			{
				Name:        "mock-node",
				Server:      upstreamAddr,
				ServerProto: "tcp",
			},
		},
		DNSListen:    dnsUDPPort,
		DoHListen:    dohTCPPort,
		Socks5Listen: socks5Port,
		HTTPListen:   httpPort,
		EnableFakeIP: true,
	}

	engine := NewEngine(engineCfg)
	if err := engine.Start(); err != nil {
		t.Fatalf("engine start failed: %v", err)
	}
	defer engine.Stop()

	time.Sleep(100 * time.Millisecond)

	// --- Test Feature 1: UDP DNS Service ---
	dnsClient := new(dns.Client)
	dnsClient.Timeout = 2 * time.Second
	qMsg := new(dns.Msg)
	qMsg.SetQuestion("test.yellowsocks.io.", dns.TypeA)
	dnsResp, _, err := dnsClient.Exchange(qMsg, dnsUDPPort)
	if err != nil {
		t.Fatalf("UDP DNS query failed: %v", err)
	}
	if len(dnsResp.Answer) == 0 {
		t.Fatalf("expected answer from UDP DNS, got 0")
	}

	// --- Test Feature 1.1: TCP DoH Service ---
	qPacked, _ := qMsg.Pack()
	b64Query := base64.RawURLEncoding.EncodeToString(qPacked)
	dohResp, err := http.Get("http://" + dohTCPPort + "/dns-query?dns=" + b64Query)
	if err != nil {
		t.Fatalf("DoH GET query failed: %v", err)
	}
	defer dohResp.Body.Close()
	if dohResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from DoH, got %d", dohResp.StatusCode)
	}
	dohBody, _ := io.ReadAll(dohResp.Body)
	dohMsg := new(dns.Msg)
	if err := dohMsg.Unpack(dohBody); err != nil {
		t.Fatalf("unpack DoH response error: %v", err)
	}
	if len(dohMsg.Answer) == 0 {
		t.Fatalf("expected answer from DoH query, got 0")
	}

	// --- Test Feature 2: Inbound SOCKS5 TCP Service ---
	s5TCPAddr, _ := net.ResolveTCPAddr("tcp", socks5Port)
	s5Conn, err := net.DialTCP("tcp", nil, s5TCPAddr)
	if err != nil {
		t.Fatalf("dial SOCKS5 failed: %v", err)
	}
	defer s5Conn.Close()

	if err := network.Sock5Handshake(s5Conn, 5000, "", ""); err != nil {
		t.Fatalf("SOCKS5 handshake failed: %v", err)
	}

	echoH, echoPStr, _ := net.SplitHostPort(tcpEchoL.Addr().String())
	echoP, _ := strconv.Atoi(echoPStr)
	if err := network.Sock5SetRequest(s5Conn, echoH, echoP, 5000); err != nil {
		t.Fatalf("SOCKS5 connect request failed: %v", err)
	}

	testPayload := []byte("hello yellowsocks unified engine")
	_, _ = s5Conn.Write(testPayload)
	echoBuf := make([]byte, len(testPayload))
	_, _ = io.ReadFull(s5Conn, echoBuf)
	if string(echoBuf) != string(testPayload) {
		t.Errorf("expected %s, got %s", testPayload, echoBuf)
	}

	// --- Test Feature 2.1: Inbound SOCKS5 UDP Service ---
	s5UDPControl, err := net.DialTCP("tcp", nil, s5TCPAddr)
	if err != nil {
		t.Fatalf("dial SOCKS5 for UDP associate failed: %v", err)
	}
	defer s5UDPControl.Close()

	if err := network.Sock5Handshake(s5UDPControl, 5000, "", ""); err != nil {
		t.Fatalf("SOCKS5 handshake failed: %v", err)
	}

	relayBnd, err := network.Sock5SetUDPRequest(s5UDPControl, "0.0.0.0", 0, 5000)
	if err != nil {
		t.Fatalf("SOCKS5 set udp request failed: %v", err)
	}
	relayUDPAddr, err := net.ResolveUDPAddr("udp", relayBnd)
	if err != nil {
		t.Fatalf("resolve relay addr error: %v", err)
	}

	uClient, err := net.ListenUDP("udp", nil)
	if err != nil {
		t.Fatalf("uClient listen error: %v", err)
	}
	defer uClient.Close()

	uEchoH, uEchoPStr, _ := net.SplitHostPort(udpEcho.LocalAddr().String())
	uEchoP, _ := strconv.Atoi(uEchoPStr)

	udpData := []byte("udp associate payload")
	packedUDP, _ := network.Sock5PackUDP(uEchoH, uEchoP, udpData)
	_, _ = uClient.WriteToUDP(packedUDP, relayUDPAddr)

	respUDPPkt := make([]byte, 65535)
	_ = uClient.SetReadDeadline(time.Now().Add(3 * time.Second))
	rn, _, err := uClient.ReadFromUDP(respUDPPkt)
	if err != nil {
		t.Fatalf("read UDP response error: %v", err)
	}
	_, _, recvPayload, err := network.Sock5UnpackUDP(respUDPPkt[:rn])
	if err != nil {
		t.Fatalf("unpack UDP response error: %v", err)
	}
	if string(recvPayload) != string(udpData) {
		t.Errorf("expected %s, got %s", udpData, recvPayload)
	}

	// --- Test Feature 2.2: Inbound HTTP CONNECT Service ---
	httpConn, err := net.Dial("tcp", httpPort)
	if err != nil {
		t.Fatalf("dial HTTP proxy failed: %v", err)
	}
	defer httpConn.Close()

	connectHeader := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n",
		tcpEchoL.Addr().String(), tcpEchoL.Addr().String())
	_, _ = httpConn.Write([]byte(connectHeader))
	br := bufio.NewReader(httpConn)
	replyLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read connect reply error: %v", err)
	}
	if !bytes.Contains([]byte(replyLine), []byte("200 Connection Established")) {
		t.Fatalf("expected 200 Connection Established, got %s", replyLine)
	}
	// Consume empty line after headers
	_, _ = br.ReadString('\n')

	// Tunnel raw bytes over CONNECT
	tunnelData := []byte("tunnel data through http proxy")
	_, _ = httpConn.Write(tunnelData)
	tunnelReply := make([]byte, len(tunnelData))
	_, _ = io.ReadFull(httpConn, tunnelReply)
	if string(tunnelReply) != string(tunnelData) {
		t.Errorf("expected %s, got %s", tunnelData, tunnelReply)
	}
}
