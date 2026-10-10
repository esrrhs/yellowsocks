package core

import (
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/esrrhs/gohome/loggo"
	appdns "github.com/esrrhs/yellowsocks/core/dns"
	"github.com/esrrhs/yellowsocks/core/proxy"
	"github.com/esrrhs/yellowsocks/core/router"
	"github.com/esrrhs/yellowsocks/core/sppclient"
)

// EngineConfig is the Linux proxy engine configuration.
type EngineConfig struct {
	SPPServer   string // remote SPP server ip:port (single node)
	SPPProto    string // tcp, udp, kcp, quic
	SPPKey      string
	SPPEncrypt  string
	SPPCompress int
	SPPNodes    []*sppclient.Node

	LocalSocks5 string // local SOCKS5 published by the SPP client

	DNSListen   string
	DoHListen   string
	DoTListen   string
	TLSCertFile string
	TLSKeyFile  string
	DirectDNS   string
	RemoteDoH   string

	Socks5Listen  string
	HTTPListen    string
	ProxyUsername string
	ProxyPassword string

	// EnableIPv6 lets IPv6 destinations go through SPP. Off by default because
	// the upstream usually has no IPv6 route.
	EnableIPv6 bool

	DirectDomains    []string
	DirectCIDRs      []string
	GeoIPFile        string
	ChinaDomainsFile string
	GFWDomainsFile   string
}

// Engine runs DNS, DoT, DoH, SOCKS5 and HTTP proxy, with SPP as the upstream.
type Engine struct {
	cfg          EngineConfig
	router       *router.Router
	dnsServer    *appdns.Server
	socks5Server *proxy.Socks5Server
	httpServer   *proxy.HTTPServer
	sppManager   *sppclient.Manager
	mu           sync.Mutex
	running      bool
}

// NewEngine builds an engine and fills defaults.
func NewEngine(cfg EngineConfig) *Engine {
	if cfg.LocalSocks5 == "" {
		cfg.LocalSocks5 = "127.0.0.1:10808"
	}
	if cfg.DNSListen == "" {
		cfg.DNSListen = "127.0.0.1:53"
	}
	if cfg.DirectDNS == "" {
		cfg.DirectDNS = "1.1.1.1:53"
	}
	if cfg.RemoteDoH == "" {
		cfg.RemoteDoH = "https://1.1.1.1/dns-query"
	}
	if cfg.SPPProto == "" {
		cfg.SPPProto = "tcp"
	}
	return &Engine{cfg: cfg}
}

// Start brings up SPP, DNS/DoH/DoT, and the inbound proxies.
func (e *Engine) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.running {
		return nil
	}

	var chinaFiles, gfwFiles []string
	if e.cfg.ChinaDomainsFile != "" {
		chinaFiles = append(chinaFiles, e.cfg.ChinaDomainsFile)
	}
	if e.cfg.GFWDomainsFile != "" {
		gfwFiles = append(gfwFiles, e.cfg.GFWDomainsFile)
	}

	e.router = router.NewRouterWithOptions(router.Options{
		DirectDomains:    e.cfg.DirectDomains,
		DirectCIDRs:      e.cfg.DirectCIDRs,
		GeoIPFile:        e.cfg.GeoIPFile,
		ChinaDomainFiles: chinaFiles,
		GFWDomainFiles:   gfwFiles,
	})

	nodes := e.cfg.SPPNodes
	if len(nodes) == 0 && e.cfg.SPPServer != "" {
		nodes = []*sppclient.Node{
			{
				Name:        "default_node",
				Server:      e.cfg.SPPServer,
				ServerProto: e.cfg.SPPProto,
				Key:         e.cfg.SPPKey,
				Encrypt:     e.cfg.SPPEncrypt,
				Compress:    e.cfg.SPPCompress,
			},
		}
	}
	if len(nodes) == 0 {
		e.cleanupServices()
		return fmt.Errorf("no SPP servers or nodes specified")
	}

	loggo.Info("[Engine] Initializing SPP manager with %d nodes...", len(nodes))
	mgr, err := sppclient.NewManager(nodes, e.cfg.LocalSocks5)
	if err != nil {
		e.cleanupServices()
		return fmt.Errorf("failed to init SPP manager: %w", err)
	}
	e.sppManager = mgr

	loggo.Info("[Engine] Starting DNS (UDP: %s, DoH: %s, DoT: %s)...",
		e.cfg.DNSListen, e.cfg.DoHListen, e.cfg.DoTListen)
	dnsSrv, err := appdns.NewServer(appdns.Config{
		ListenAddr:    e.cfg.DNSListen,
		DoHListenAddr: e.cfg.DoHListen,
		DoTListenAddr: e.cfg.DoTListen,
		TLSCertFile:   e.cfg.TLSCertFile,
		TLSKeyFile:    e.cfg.TLSKeyFile,
		DoHURL:        e.cfg.RemoteDoH,
		DirectDNS:     e.cfg.DirectDNS,
		Socks5Addr:    e.sppManager.Socks5Addr(),
		Router:        e.router,
		EnableIPv6:    e.cfg.EnableIPv6,
	})
	if err != nil {
		e.cleanupServices()
		return fmt.Errorf("failed to init DNS server: %w", err)
	}
	// SPP node hostnames must resolve and connect directly, never via SPP.
	for _, n := range nodes {
		host := sppHost(n.Server)
		if host != "" && net.ParseIP(host) == nil {
			e.router.AddDirectDomain(host)
		}
	}
	if err := dnsSrv.Start(); err != nil {
		// Start may have bound some listeners (UDP/TCP/DoH) before failing;
		// release them along with the SPP manager and router instead of
		// leaking sockets and the GeoIP mapping.
		_ = dnsSrv.Stop()
		e.cleanupServices()
		if strings.Contains(err.Error(), "address already in use") {
			return fmt.Errorf("DNS 端口已被占用: %w", err)
		}
		return fmt.Errorf("failed to run DNS server: %w", err)
	}
	e.dnsServer = dnsSrv

	if e.cfg.Socks5Listen != "" {
		loggo.Info("[Engine] Starting SOCKS5 on %s...", e.cfg.Socks5Listen)
		s5 := proxy.NewSocks5Server(proxy.Socks5Config{
			ListenAddr: e.cfg.Socks5Listen,
			Username:   e.cfg.ProxyUsername,
			Password:   e.cfg.ProxyPassword,
			Router:     e.router,
			DNS:        e.dnsServer,
			Upstream:   e.sppManager,
			EnableIPv6: e.cfg.EnableIPv6,
		})
		if err := s5.Start(); err != nil {
			e.cleanupServices()
			return fmt.Errorf("start SOCKS5: %w", err)
		}
		e.socks5Server = s5
	}

	if e.cfg.HTTPListen != "" {
		loggo.Info("[Engine] Starting HTTP proxy on %s...", e.cfg.HTTPListen)
		hSrv := proxy.NewHTTPServer(proxy.HTTPConfig{
			ListenAddr: e.cfg.HTTPListen,
			Username:   e.cfg.ProxyUsername,
			Password:   e.cfg.ProxyPassword,
			Router:     e.router,
			DNS:        e.dnsServer,
			Upstream:   e.sppManager,
			EnableIPv6: e.cfg.EnableIPv6,
		})
		if err := hSrv.Start(); err != nil {
			e.cleanupServices()
			return fmt.Errorf("start HTTP proxy: %w", err)
		}
		e.httpServer = hSrv
	}

	e.running = true
	loggo.Info("[Engine] YellowSocks is up")
	return nil
}

func sppHost(server string) string {
	host, _, err := net.SplitHostPort(server)
	if err != nil || host == "" {
		host = server
	}
	return strings.ToLower(strings.Trim(host, "."))
}

// cleanupServices releases every resource Start acquired, including the
// router's GeoIP mapping. It is used both on normal shutdown and on failed
// startup, so any partially built engine leaves nothing behind.
func (e *Engine) cleanupServices() {
	if e.socks5Server != nil {
		_ = e.socks5Server.Stop()
		e.socks5Server = nil
	}
	if e.httpServer != nil {
		_ = e.httpServer.Stop()
		e.httpServer = nil
	}
	if e.dnsServer != nil {
		_ = e.dnsServer.Stop()
		e.dnsServer = nil
	}
	if e.sppManager != nil {
		e.sppManager.Close()
		e.sppManager = nil
	}
	if e.router != nil {
		e.router.Close()
		e.router = nil
	}
}

// Stop shuts the engine down. It is safe to call after a failed Start: all
// resources acquired up to the failure have already been released, and the
// call is a no-op.
func (e *Engine) Stop() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.running {
		// A failed Start cleans up after itself; nothing remains to stop.
		return nil
	}
	e.running = false

	loggo.Info("[Engine] Shutting down...")
	e.cleanupServices()
	loggo.Info("[Engine] Stopped")
	return nil
}
