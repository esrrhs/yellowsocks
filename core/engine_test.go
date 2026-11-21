package core

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/gohome/network"
	"github.com/esrrhs/yellowsocks/core/sppclient"
)

// startFakeSPPListener accepts TCP connections and discards them. It is only
// used to make startup node probes succeed quickly; no SPP protocol is spoken.
func startFakeSPPListener(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake spp listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			}(c)
		}
	}()
	return l
}

func baseTestEngineConfig(sppAddr string) EngineConfig {
	return EngineConfig{
		SPPProto:     "tcp",
		SPPServer:    sppAddr,
		SPPKey:       "testkey",
		LocalSocks5:  "127.0.0.1:0",
		DNSListen:    "127.0.0.1:0",
		Socks5Listen: "127.0.0.1:0",
		HTTPListen:   "127.0.0.1:0",
		DirectDNS:    "127.0.0.1:1",
		RemoteDoH:    "http://127.0.0.1:1/dns-query",
	}
}

// A failed Start must release everything it allocated, including the router
// (GeoIP mapping). Previously the router leaked, and a later Stop was a no-op
// because running stayed false.
func TestEngineStartFailureReleasesResources(t *testing.T) {
	fakeSPP := startFakeSPPListener(t)

	// Occupy a UDP port so the DNS server fails to bind at Start.
	blocked, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("block udp: %v", err)
	}
	defer blocked.Close()

	cfg := baseTestEngineConfig(fakeSPP.Addr().String())
	cfg.DNSListen = blocked.LocalAddr().String()
	e := NewEngine(cfg)

	err = e.Start()
	if err == nil {
		t.Fatal("expected Start to fail on occupied DNS port")
	}
	if !strings.Contains(err.Error(), "listen udp") &&
		!strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.router != nil || e.sppManager != nil || e.dnsServer != nil {
		t.Fatalf("resources leaked after failed Start: router=%v spp=%v dns=%v",
			e.router != nil, e.sppManager != nil, e.dnsServer != nil)
	}

	// Stop after a failed Start must be a safe no-op.
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop after failed Start: %v", err)
	}

	// Once the blocked port is gone, the same engine must start cleanly:
	// cleanup cannot leave the engine in a half-initialized state.
	_ = blocked.Close()
	if err := e.Start(); err != nil {
		t.Fatalf("restart after failed Start: %v", err)
	}
	if e.router == nil || e.sppManager == nil || e.dnsServer == nil {
		t.Fatal("resources missing after successful restart")
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if e.router != nil || e.sppManager != nil || e.dnsServer != nil ||
		e.socks5Server != nil || e.httpServer != nil {
		t.Fatal("resources retained after Stop")
	}
	// Stop must be idempotent.
	if err := e.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// The no-upstream early return also used to leak the freshly built router.
func TestEngineStartWithoutNodesCleansUp(t *testing.T) {
	cfg := baseTestEngineConfig("")
	cfg.SPPServer = ""
	cfg.SPPNodes = nil
	e := NewEngine(cfg)
	err := e.Start()
	if err == nil || !strings.Contains(err.Error(), "no SPP") {
		t.Fatalf("want no-SPP error, got %v", err)
	}
	if e.router != nil {
		t.Fatal("router leaked after no-SPP failure")
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// A normal Start/Stop cycle tears down every component.
func TestEngineStartStopCycle(t *testing.T) {
	fakeSPP := startFakeSPPListener(t)
	cfg := baseTestEngineConfig(fakeSPP.Addr().String())
	cfg.SPPNodes = []*sppclient.Node{{
		Name:        "n1",
		Server:      fakeSPP.Addr().String(),
		ServerProto: "tcp",
		Key:         "testkey",
	}}

	e := NewEngine(cfg)
	if err := e.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !e.running {
		t.Fatal("running flag not set")
	}
	// A second Start while running is a harmless no-op.
	if err := e.Start(); err != nil {
		t.Fatalf("re-Start: %v", err)
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if e.running {
		t.Fatal("running flag still set after Stop")
	}
}

// The statistics API is wired by the engine: it starts with the other
// services, answers with the configured token, and is released on Stop.
func TestEngineStatisticsAPI(t *testing.T) {
	fakeSPP := startFakeSPPListener(t)
	cfg := baseTestEngineConfig(fakeSPP.Addr().String())
	cfg.APIListen = "127.0.0.1:0"
	cfg.APIToken = "engine-token"
	cfg.APIDashboard = true
	cfg.SPPNodes = []*sppclient.Node{{
		Name:        "n1",
		Server:      fakeSPP.Addr().String(),
		ServerProto: "tcp",
		Key:         "testkey",
	}}

	e := NewEngine(cfg)
	if err := e.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = e.Stop() }()

	if e.apiServer == nil || e.collector == nil {
		t.Fatal("API server or collector not created")
	}
	addr := e.apiServer.Addr().String()
	base := "http://" + addr

	// Engine state reaches the API: listeners, routing rules and nodes.
	info := e.APIInfo()
	if info.Listeners.API != cfg.APIListen || info.Listeners.DNS != cfg.DNSListen {
		t.Fatalf("listeners = %+v", info.Listeners)
	}
	if len(info.Nodes) != 1 || info.Nodes[0].Name != "n1" {
		t.Fatalf("nodes = %+v", info.Nodes)
	}
	if info.Router.DirectDomains == 0 {
		t.Fatal("built-in direct domains missing from router info")
	}

	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/overview", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("overview without token: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("overview without token -> %d, want 401", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodGet, base+"/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer engine-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status -> %d, want 200", resp.StatusCode)
	}
	var status struct {
		Version   string `json:"version"`
		Running   bool   `json:"running"`
		Listeners struct {
			API string `json:"api"`
		} `json:"listeners"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !status.Running || status.Version == "" {
		t.Fatalf("status = %+v", status)
	}

	if err := e.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if e.apiServer != nil || e.collector != nil {
		t.Fatal("API server or collector retained after Stop")
	}
}

// startFakeSocks5Upstream runs a minimal RFC 1928 server that requires
// username/password auth and relays CONNECT to the real target. It emulates
// any generic SOCKS5 proxy (e.g. a remote clash/v2ray inbound). dialOverride
// lets the test map a fake domain onto a local listener; upstreamTarget is
// fed the host:port of every CONNECT that reached the upstream.
func startFakeSocks5Upstream(t *testing.T, user, pass string, dialOverride func(hostport string) string) (addr string, stop func(), upstreamTarget <-chan string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake socks5 upstream listen: %v", err)
	}
	targets := make(chan string, 16)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if err := network.Sock5HandshakeBy(c, user, pass); err != nil {
					return
				}
				cmd, _, hostport, err := network.Sock5GetRequest(c)
				if err != nil || cmd != network.Socks5CmdConnect {
					return
				}
				select {
				case targets <- hostport:
				default:
				}
				if dialOverride != nil {
					if mapped := dialOverride(hostport); mapped != "" {
						hostport = mapped
					}
				}
				dst, err := net.DialTimeout("tcp", hostport, 5*time.Second)
				if err != nil {
					_ = network.Sock5SendConnectReply(c, 0x05, "0.0.0.0:0")
					return
				}
				defer dst.Close()
				_ = network.Sock5SendConnectReply(c, 0x00, "0.0.0.0:0")
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(dst, c); done <- struct{}{} }()
				go func() { _, _ = io.Copy(c, dst); done <- struct{}{} }()
				<-done
			}(c)
		}
	}()
	return l.Addr().String(), func() { _ = l.Close() }, targets
}

// TestEngineSocks5UpstreamEndToEnd drives a full relay chain through an
// engine configured with spp_proto "socks5": inbound SOCKS5 -> static
// upstream (auth) -> fake upstream -> echo target. It proves the engine
// forwards proxied traffic to a plain SOCKS5 server without the SPP stack.
func TestEngineSocks5UpstreamEndToEnd(t *testing.T) {
	const upUser, upPass = "upuser", "uppass"

	echoL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer echoL.Close()
	go func() {
		for {
			c, err := echoL.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_, _ = io.Copy(c, c)
				_ = c.Close()
			}(c)
		}
	}()

	upAddr, stopUp, upstreamTarget := startFakeSocks5Upstream(t, upUser, upPass, func(hostport string) string {
		if strings.HasPrefix(hostport, "upstream.test:") {
			return echoL.Addr().String()
		}
		return ""
	})
	defer stopUp()

	s5L, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve socks5 port: %v", err)
	}
	s5Addr := s5L.Addr().String()
	_ = s5L.Close()

	e := NewEngine(EngineConfig{
		SPPProto:     "socks5",
		SPPServer:    upAddr,
		SPPUsername:  upUser,
		SPPPassword:  upPass,
		DNSListen:    "127.0.0.1:0",
		Socks5Listen: s5Addr,
		HTTPListen:   "",
		DirectDNS:    "127.0.0.1:1",
		RemoteDoH:    "http://127.0.0.1:1/dns-query",
	})
	if err := e.Start(); err != nil {
		t.Fatalf("engine start with socks5 upstream: %v", err)
	}
	defer func() { _ = e.Stop() }()

	client, err := net.Dial("tcp", s5Addr)
	if err != nil {
		t.Fatalf("dial engine inbound: %v", err)
	}
	defer client.Close()
	tcpClient := client.(*net.TCPConn)
	if err := network.Sock5Handshake(tcpClient, 5000, "", ""); err != nil {
		t.Fatalf("engine inbound handshake: %v", err)
	}
	// A domain target: unknown domains route to Proxy, so the traffic must
	// reach the fake upstream (loopback IPs would go direct by built-in
	// reserved-range rules).
	if err := network.Sock5SetRequest(tcpClient, "upstream.test", 443, 5000); err != nil {
		t.Fatalf("connect via engine: %v", err)
	}
	select {
	case got := <-upstreamTarget:
		if got != "upstream.test:443" {
			t.Fatalf("upstream saw target %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("traffic never reached the socks5 upstream")
	}

	msg := []byte("hello through socks5 upstream")
	if _, err := client.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(msg) {
		t.Fatalf("echo mismatch: %q", got)
	}

	// The engine must not have created an SPP manager for this config.
	if e.sppManager != nil {
		t.Fatal("spp manager created for socks5 upstream")
	}
}

// TestEngineSocks5UpstreamRequiresServer checks the config validation.
func TestEngineSocks5UpstreamRequiresServer(t *testing.T) {
	e := NewEngine(EngineConfig{
		SPPProto:     "socks5",
		SPPServer:    "",
		DNSListen:    "127.0.0.1:0",
		Socks5Listen: "127.0.0.1:0",
		HTTPListen:   "",
	})
	err := e.Start()
	if err == nil {
		_ = e.Stop()
		t.Fatal("start with socks5 upstream and no server must fail")
	}
	if !strings.Contains(err.Error(), "socks5 upstream requires spp_server") {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.running {
		t.Fatal("engine must not be running after failed start")
	}
}
