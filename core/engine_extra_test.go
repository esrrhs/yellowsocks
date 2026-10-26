package core

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esrrhs/yellowsocks/core/sppclient"
)

// nodeServerAddr renders the fake SPP listener address with a hostname so the
// engine must special-case it as a direct domain (sppHost + AddDirectDomain).
func nodeServerAddr(t *testing.T, l net.Listener) string {
	t.Helper()
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("split fake spp addr: %v", err)
	}
	return net.JoinHostPort("localhost", port)
}

// occupyTCP holds a TCP port for the duration of the test so a component
// configured with the returned address fails to bind.
func occupyTCP(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy tcp: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.Addr().String()
}

func TestNewEngineDefaults(t *testing.T) {
	e := NewEngine(EngineConfig{})
	if e.cfg.LocalSocks5 != "127.0.0.1:10808" {
		t.Fatalf("LocalSocks5 default = %s", e.cfg.LocalSocks5)
	}
	if e.cfg.DNSListen != "127.0.0.1:53" {
		t.Fatalf("DNSListen default = %s", e.cfg.DNSListen)
	}
	if e.cfg.DirectDNS != "1.1.1.1:53" {
		t.Fatalf("DirectDNS default = %s", e.cfg.DirectDNS)
	}
	if e.cfg.RemoteDoH != "https://1.1.1.1/dns-query" {
		t.Fatalf("RemoteDoH default = %s", e.cfg.RemoteDoH)
	}
	if e.cfg.SPPProto != "tcp" {
		t.Fatalf("SPPProto default = %s", e.cfg.SPPProto)
	}
}

func TestHistoryMinutes(t *testing.T) {
	cases := []struct {
		hours int
		want  int
	}{
		{0, 1440},                   // unset falls back to 24h
		{-5, 1440},                  // negative falls back too
		{1, 60},                     // one hour
		{48, 48 * 60},               // two days
		{24 * 30, 24 * 30 * 60},     // exactly the cap
		{24*30 + 100, 24 * 30 * 60}, // capped at 30 days
	}
	for _, c := range cases {
		if got := historyMinutes(c.hours); got != c.want {
			t.Fatalf("historyMinutes(%d) = %d, want %d", c.hours, got, c.want)
		}
	}
}

func TestSPPHost(t *testing.T) {
	cases := []struct {
		server string
		want   string
	}{
		{"1.2.3.4:8443", "1.2.3.4"},
		{"portal.example.com:443", "portal.example.com"},
		{"Example.COM.", "example.com"},      // no port: kept whole, trimmed, lowercased
		{"[::1]:53", "::1"},                  // bracketed IPv6
		{"Main.Site.IO:80.", "main.site.io"}, // trailing dot on the host part
		{":53", ":53"},                       // empty host: falls back to the raw server
		{"", ""},                             // nothing configured
	}
	for _, c := range cases {
		if got := sppHost(c.server); got != c.want {
			t.Fatalf("sppHost(%q) = %q, want %q", c.server, got, c.want)
		}
	}
}

// Stats and activeNodeName must work both before Start (nil-safe) and while
// running, and the configured StatsHistoryHours must reach the collector.
func TestEngineStatsAndActiveNode(t *testing.T) {
	fakeSPP := startFakeSPPListener(t)

	chinaFile := filepath.Join(t.TempDir(), "china.txt")
	if err := os.WriteFile(chinaFile, []byte("china.example.com\n"), 0o600); err != nil {
		t.Fatalf("write china list: %v", err)
	}
	gfwFile := filepath.Join(t.TempDir(), "gfw.txt")
	if err := os.WriteFile(gfwFile, []byte("gfw.example.com\n"), 0o600); err != nil {
		t.Fatalf("write gfw list: %v", err)
	}

	cfg := baseTestEngineConfig(fakeSPP.Addr().String())
	cfg.StatsHistoryHours = 2
	cfg.ChinaDomainsFile = chinaFile
	cfg.GFWDomainsFile = gfwFile
	// A hostname node exercises the direct-domain exception for SPP servers
	// that are not bare IPs.
	cfg.SPPNodes = []*sppclient.Node{{
		Name:        "n1",
		Server:      nodeServerAddr(t, fakeSPP),
		ServerProto: "tcp",
		Key:         "testkey",
	}}

	e := NewEngine(cfg)

	// Before Start there is no collector and no upstream.
	if got := e.Stats(); got != nil {
		t.Fatal("Stats before Start should be nil")
	}
	if got := e.activeNodeName(); got != "" {
		t.Fatalf("activeNodeName before Start = %q, want empty", got)
	}

	if err := e.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	c := e.Stats()
	if c == nil {
		t.Fatal("Stats after Start is nil")
	}
	if got := c.Config().HistoryMinutes; got != 120 {
		t.Fatalf("collector history = %d minutes, want 120 (2h)", got)
	}

	// The returned collector is live: errors recorded through it show up.
	e.Stats().RecordError("engine", "host.test", "boom")
	if got := c.Overview().Totals.Errors; got != 1 {
		t.Fatalf("errors through engine Stats = %d, want 1", got)
	}

	if got := e.activeNodeName(); got != "n1" {
		t.Fatalf("activeNodeName = %q, want n1", got)
	}

	// The hostname SPP server must be exempted from proxying.
	if !e.router.ShouldDirectDomain("localhost") {
		t.Fatal("SPP node host not added as a direct domain")
	}

	if err := e.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := e.Stats(); got != nil {
		t.Fatal("Stats after Stop should be nil")
	}
	if got := e.activeNodeName(); got != "" {
		t.Fatalf("activeNodeName after Stop = %q, want empty", got)
	}
}

// A DoT listener without TLS material fails before anything is bound.
func TestEngineStartDNSInitError(t *testing.T) {
	fakeSPP := startFakeSPPListener(t)
	cfg := baseTestEngineConfig(fakeSPP.Addr().String())
	cfg.DoTListen = "127.0.0.1:0"

	e := NewEngine(cfg)
	err := e.Start()
	if err == nil || !strings.Contains(err.Error(), "failed to init DNS server") {
		t.Fatalf("want DNS init error, got %v", err)
	}
	if e.dnsServer != nil || e.sppManager != nil || e.router != nil {
		t.Fatal("resources leaked after DNS init failure")
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop after failed Start: %v", err)
	}
}

// A DoT listener with unreadable TLS material fails while starting the DNS
// server, and the error must not be mistaken for a port conflict.
func TestEngineStartDNSRunError(t *testing.T) {
	fakeSPP := startFakeSPPListener(t)
	cfg := baseTestEngineConfig(fakeSPP.Addr().String())
	cfg.DoTListen = "127.0.0.1:0"
	dir := t.TempDir()
	cfg.TLSCertFile = filepath.Join(dir, "missing.crt")
	cfg.TLSKeyFile = filepath.Join(dir, "missing.key")

	e := NewEngine(cfg)
	err := e.Start()
	if err == nil || !strings.Contains(err.Error(), "failed to run DNS server") {
		t.Fatalf("want DNS run error, got %v", err)
	}
	if strings.Contains(err.Error(), "端口已被占用") {
		t.Fatalf("TLS failure misreported as port conflict: %v", err)
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop after failed Start: %v", err)
	}
}

// A DNS port already taken must surface as the friendly port-conflict error.
func TestEngineStartDNSPortInUse(t *testing.T) {
	fakeSPP := startFakeSPPListener(t)
	occupied := occupyTCP(t)

	cfg := baseTestEngineConfig(fakeSPP.Addr().String())
	cfg.DNSListen = occupied

	e := NewEngine(cfg)
	err := e.Start()
	if err == nil || !strings.Contains(err.Error(), "DNS 端口已被占用") {
		t.Fatalf("want DNS port-conflict error, got %v", err)
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop after failed Start: %v", err)
	}
}

// Inbound proxies that cannot bind abort startup with a dedicated error.
func TestEngineStartProxyPortInUse(t *testing.T) {
	fakeSPP := startFakeSPPListener(t)

	socksPort := occupyTCP(t)
	cfg := baseTestEngineConfig(fakeSPP.Addr().String())
	cfg.Socks5Listen = socksPort
	e := NewEngine(cfg)
	err := e.Start()
	if err == nil || !strings.Contains(err.Error(), "start SOCKS5") {
		t.Fatalf("want SOCKS5 start error, got %v", err)
	}
	if e.socks5Server != nil || e.dnsServer != nil || e.sppManager != nil {
		t.Fatal("resources leaked after SOCKS5 start failure")
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop after failed Start: %v", err)
	}

	httpPort := occupyTCP(t)
	cfg = baseTestEngineConfig(fakeSPP.Addr().String())
	cfg.HTTPListen = httpPort
	e = NewEngine(cfg)
	err = e.Start()
	if err == nil || !strings.Contains(err.Error(), "start HTTP proxy") {
		t.Fatalf("want HTTP proxy start error, got %v", err)
	}
	if e.httpServer != nil || e.dnsServer != nil || e.sppManager != nil {
		t.Fatal("resources leaked after HTTP proxy start failure")
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop after failed Start: %v", err)
	}
}

// A malformed CIDR fails while building the API, an occupied port fails while
// starting it; both must tear down the rest of the engine.
func TestEngineStartAPIErrors(t *testing.T) {
	fakeSPP := startFakeSPPListener(t)

	cfg := baseTestEngineConfig(fakeSPP.Addr().String())
	cfg.APIListen = "127.0.0.1:0"
	cfg.APIAllowedCIDRs = []string{"not-a-cidr"}
	e := NewEngine(cfg)
	err := e.Start()
	if err == nil || !strings.Contains(err.Error(), "init statistics API") {
		t.Fatalf("want API init error, got %v", err)
	}
	if e.apiServer != nil || e.collector != nil || e.dnsServer != nil {
		t.Fatal("resources leaked after API init failure")
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop after failed Start: %v", err)
	}

	occupied := occupyTCP(t)
	cfg = baseTestEngineConfig(fakeSPP.Addr().String())
	cfg.APIListen = occupied
	e = NewEngine(cfg)
	err = e.Start()
	if err == nil || !strings.Contains(err.Error(), "start statistics API") {
		t.Fatalf("want API start error, got %v", err)
	}
	if e.apiServer != nil || e.collector != nil || e.dnsServer != nil {
		t.Fatal("resources leaked after API start failure")
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop after failed Start: %v", err)
	}
}
