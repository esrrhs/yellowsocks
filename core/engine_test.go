package core

import (
	"io"
	"net"
	"strings"
	"testing"

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
