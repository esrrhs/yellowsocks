package sppclient

import (
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	sppproxy "github.com/esrrhs/spp/proxy"
	"golang.org/x/net/proxy"
)

// TestRUDP_E2E_StreamAcrossHeartbeat verifies that a real RUDP-backed SPP client
// establishes connectivity, handles heartbeats across the 1-second heartbeat interval,
// and continues to reliably proxy application traffic without stalls or sequence corruption.
func TestRUDP_E2E_StreamAcrossHeartbeat(t *testing.T) {
	// 1. Target Echo Server
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo: %v", err)
	}
	defer echoLn.Close()

	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()

	// 2. Allocate UDP port for SPP RUDP Server
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	serverPort := pc.LocalAddr().(*net.UDPAddr).Port
	_ = pc.Close()
	serverAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(serverPort))

	// 3. Start local SPP RUDP Server
	serverCfg := sppproxy.DefaultConfig()
	serverCfg.Key = "e2e-rudp-test-key"
	serverCfg.Encrypt = ""
	serverCfg.EncryptType = sppproxy.EncryptNone
	serverCfg.CompressType = sppproxy.CompressNone
	serverCfg.AuthTimeout = 5

	srv, err := sppproxy.NewServer(serverCfg, []string{"rudp"}, []string{serverAddr})
	if err != nil {
		t.Fatalf("NewServer rudp: %v", err)
	}
	defer srv.Close()

	// 4. Allocate TCP port for local SOCKS5 listener
	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen socks5 port: %v", err)
	}
	socksPort := socksLn.Addr().(*net.TCPAddr).Port
	_ = socksLn.Close()
	socks5Addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(socksPort))

	// 5. Start Yellowsocks SPP Client via RUDP
	clientCfg := &Config{
		ServerProto: "rudp",
		Server:      serverAddr,
		Key:         "e2e-rudp-test-key",
		LocalSocks5: socks5Addr,
	}
	cl, err := NewClient(clientCfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cl.Close()

	// 6. Wait for SOCKS5 listener and tunnel to become ready
	deadline := time.Now().Add(5 * time.Second)
	var dialer proxy.Dialer
	for time.Now().Before(deadline) {
		d, err := proxy.SOCKS5("tcp", socks5Addr, nil, proxy.Direct)
		if err == nil {
			testConn, err := d.Dial("tcp", echoLn.Addr().String())
			if err == nil {
				_ = testConn.Close()
				dialer = d
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if dialer == nil {
		t.Fatal("SOCKS5 client did not become ready within deadline")
	}

	// 7. Open connection through the RUDP SOCKS5 proxy to the echo server
	conn, err := dialer.Dial("tcp", echoLn.Addr().String())
	if err != nil {
		t.Fatalf("dial echo server via socks5: %v", err)
	}
	defer conn.Close()

	// 8. Send payload before heartbeat
	payload1 := []byte("hello-before-heartbeat")
	if _, err := conn.Write(payload1); err != nil {
		t.Fatalf("write payload1: %v", err)
	}
	buf1 := make([]byte, len(payload1))
	if _, err := io.ReadFull(conn, buf1); err != nil {
		t.Fatalf("read payload1: %v", err)
	}
	if string(buf1) != string(payload1) {
		t.Fatalf("payload1 mismatch: got %q, want %q", string(buf1), string(payload1))
	}

	// 9. Sleep longer than the 1-second heartbeat interval so heartbeats flow over RUDP
	time.Sleep(1500 * time.Millisecond)

	// 10. Send payload after heartbeat and ensure full bidirectional communication
	payload2 := []byte("hello-after-heartbeat-continuous-stream-check")
	if _, err := conn.Write(payload2); err != nil {
		t.Fatalf("write payload2: %v", err)
	}
	buf2 := make([]byte, len(payload2))
	if _, err := io.ReadFull(conn, buf2); err != nil {
		t.Fatalf("read payload2: %v", err)
	}
	if string(buf2) != string(payload2) {
		t.Fatalf("payload2 mismatch: got %q, want %q", string(buf2), string(payload2))
	}
}
