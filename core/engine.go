package core

import (
	"fmt"
	"net"
	"strconv"
	"sync"

	"github.com/esrrhs/gohome/loggo"
	tun2socksCore "github.com/xjasonlyu/tun2socks/v2/core"
	"github.com/xjasonlyu/tun2socks/v2/core/device"
	"github.com/xjasonlyu/tun2socks/v2/core/device/fdbased"
	"github.com/xjasonlyu/tun2socks/v2/core/device/tun"
	gvisorStack "gvisor.dev/gvisor/pkg/tcpip/stack"
	appdns "github.com/esrrhs/yellowsocks/core/dns"
	"github.com/esrrhs/yellowsocks/core/proxy"
	"github.com/esrrhs/yellowsocks/core/router"
	"github.com/esrrhs/yellowsocks/core/sppclient"
	"github.com/esrrhs/yellowsocks/core/tunnel"
)

// EngineConfig 核心配置
type EngineConfig struct {
	TunName          string
	TunFd            int               // 传入的已有 TUN 文件描述符 (如 Android VpnService 提供，>0 时生效)
	TunIP            string
	TunGateway       string
	TunMask          string
	MTU              int
	DisableTun       bool              // 是否禁用 TUN (仅运行代理和 DNS 服务)
	SPPServer        string            // 远端 SPP 服务器 IP:Port (单节点向后兼容)
	SPPProto         string            // SPP 协议类型 (tcp/udp/quic等)
	SPPKey           string
	SPPEncrypt       string
	SPPCompress      int
	SPPNodes         []*sppclient.Node // 多节点列表
	EnableFakeIP     bool              // 开启 Fake-IP 秒开模式
	BypassApps       []string          // 排除直连的应用名称列表 (TUN Bypass Apps)
	LocalSocks5      string            // 本地 SPP 暴露给内核的 socks5 端口 (内部中转)
	DNSListen        string            // DNS UDP listen address (default: 127.0.0.1:53)
	DoHListen        string            // DNS TCP DoH listen address (default: 127.0.0.1:8053)
	DirectDNS        string            // Direct resolver for local/domestic domain resolution
	RemoteDoH        string            // Remote DoH endpoint routed via SPP (default: https://1.1.1.1/dns-query)
	SetAutoRoute     bool              // Automatically manage system global routes
	Socks5Listen     string            // Inbound SOCKS5 proxy address (default: 127.0.0.1:1080)
	HTTPListen       string            // Inbound HTTP/HTTPS proxy address (default: 127.0.0.1:8080)
	ProxyUsername    string            // Inbound proxies auth username
	ProxyPassword    string            // Inbound proxies auth password
	DirectDomains    []string          // Custom direct domains
	DirectCIDRs      []string          // Custom direct CIDRs
	GeoIPFile        string            // GeoLite2-Country.mmdb path
	ChinaDomainsFile string            // China domain list file
	GFWDomainsFile   string            // GFW blocked domain list file
}

// Engine 统一网络引擎
type Engine struct {
	cfg          EngineConfig
	stack        *gvisorStack.Stack
	router       *router.Router
	dnsServer    *appdns.Server
	socks5Server *proxy.Socks5Server
	httpServer   *proxy.HTTPServer
	sppManager   *sppclient.Manager
	mu           sync.Mutex
	running      bool
}

// NewEngine 构建引擎实例
func NewEngine(cfg EngineConfig) *Engine {
	if cfg.TunName == "" {
		cfg.TunName = "tun0"
	}
	if cfg.TunIP == "" {
		cfg.TunIP = "10.255.0.2"
	}
	if cfg.TunGateway == "" {
		cfg.TunGateway = "10.255.0.1"
	}
	if cfg.TunMask == "" {
		cfg.TunMask = "255.255.255.0"
	}
	if cfg.MTU <= 0 {
		cfg.MTU = 1500
	}
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

	return &Engine{
		cfg: cfg,
	}
}

// Start 启动完整分流内核
func (e *Engine) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.running {
		return nil
	}

	// 1. 初始化 Router (支持 GeoIP, 域名分流, 私有IP, 应用排除)
	loggo.Info("[Engine] Initializing Router (Bypass Apps: %v)...", e.cfg.BypassApps)
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
	for _, app := range e.cfg.BypassApps {
		e.router.AddBypassApp(app)
	}

	// 2. 整理 SPP 节点列表并启动 SPP 多节点故障转移管理器
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
		return fmt.Errorf("no SPP servers or nodes specified")
	}

	loggo.Info("[Engine] Initializing SPP Multi-node Manager with %d nodes...", len(nodes))
	mgr, err := sppclient.NewManager(nodes, e.cfg.LocalSocks5)
	if err != nil {
		return fmt.Errorf("failed to init SPP Manager: %w", err)
	}
	e.sppManager = mgr

	// 3. 启动本地 DNS 服务 (UDP 端口提供快速准确 DNS + TCP 端口提供 DoH 服务)
	loggo.Info("[Engine] Starting DNS Server (UDP: %s, DoH TCP: %s, Fake-IP: %v)...",
		e.cfg.DNSListen, e.cfg.DoHListen, e.cfg.EnableFakeIP)
	dnsSrv, err := appdns.NewServer(appdns.Config{
		ListenAddr:    e.cfg.DNSListen,
		DoHListenAddr: e.cfg.DoHListen,
		DoHURL:        e.cfg.RemoteDoH,
		DirectDNS:     e.cfg.DirectDNS,
		Socks5Addr:    e.sppManager.Socks5Addr(),
		Router:        e.router,
		EnableFakeIP:  e.cfg.EnableFakeIP,
	})
	if err != nil {
		e.sppManager.Close()
		return fmt.Errorf("failed to init DNS server: %w", err)
	}
	if err := dnsSrv.Start(); err != nil {
		e.sppManager.Close()
		return fmt.Errorf("failed to run DNS server: %w", err)
	}
	e.dnsServer = dnsSrv

	// 4. 启动 SOCKS5 代理服务 (支持 TCP + UDP，智能分流，SPP转发)
	if e.cfg.Socks5Listen != "" {
		loggo.Info("[Engine] Starting Inbound SOCKS5 Proxy Server on %s (TCP+UDP)...", e.cfg.Socks5Listen)
		s5 := proxy.NewSocks5Server(proxy.Socks5Config{
			ListenAddr: e.cfg.Socks5Listen,
			Username:   e.cfg.ProxyUsername,
			Password:   e.cfg.ProxyPassword,
			Router:     e.router,
			DNS:        e.dnsServer,
			Upstream:   e.sppManager,
		})
		if err := s5.Start(); err != nil {
			loggo.Warn("[Engine] Start SOCKS5 proxy failed: %v", err)
		} else {
			e.socks5Server = s5
		}
	}

	// 5. 启动 HTTP / HTTPS 代理服务 (智能分流，SPP转发)
	if e.cfg.HTTPListen != "" {
		loggo.Info("[Engine] Starting Inbound HTTP Proxy Server on %s...", e.cfg.HTTPListen)
		hSrv := proxy.NewHTTPServer(proxy.HTTPConfig{
			ListenAddr: e.cfg.HTTPListen,
			Username:   e.cfg.ProxyUsername,
			Password:   e.cfg.ProxyPassword,
			Router:     e.router,
			DNS:        e.dnsServer,
			Upstream:   e.sppManager,
		})
		if err := hSrv.Start(); err != nil {
			loggo.Warn("[Engine] Start HTTP proxy failed: %v", err)
		} else {
			e.httpServer = hSrv
		}
	}

	// 6. 底层 TUN 机制代理全部机器流量 (如果未显式禁用 TUN)
	if !e.cfg.DisableTun {
		var dev device.Device
		if e.cfg.TunFd > 0 {
			loggo.Info("[Engine] Opening TUN from existing file descriptor %d (Android mode)...", e.cfg.TunFd)
			fdev, err := fdbased.Open(strconv.Itoa(e.cfg.TunFd), uint32(e.cfg.MTU), 0)
			if err != nil {
				e.cleanupServices()
				return fmt.Errorf("failed to open TUN from fd %d: %w", e.cfg.TunFd, err)
			}
			dev = fdev
		} else {
			loggo.Info("[Engine] Opening TUN virtual network device %s...", e.cfg.TunName)
			tdev, err := tun.Open(e.cfg.TunName, uint32(e.cfg.MTU))
			if err != nil {
				e.cleanupServices()
				return fmt.Errorf("failed to open TUN device: %w", err)
			}
			dev = tdev
		}

		handler := tunnel.NewHandler(e.router, e.dnsServer, e.sppManager)
		stack, err := tun2socksCore.CreateStack(&tun2socksCore.Config{
			LinkEndpoint:     dev,
			TransportHandler: handler,
		})
		if err != nil {
			dev.Close()
			e.cleanupServices()
			return fmt.Errorf("failed to create network stack: %w", err)
		}
		e.stack = stack

		// 自动配置系统路由
		if e.cfg.SetAutoRoute {
			sppHost, _, _ := net.SplitHostPort(e.cfg.SPPServer)
			if sppHost == "" {
				sppHost = e.cfg.SPPServer
			}
			sppIPs, _ := net.LookupIP(sppHost)
			var serverIP string
			if len(sppIPs) > 0 {
				serverIP = sppIPs[0].String()
			}
			loggo.Info("[Engine] Setting up system routes via %s (SPP IP: %s)...", e.cfg.TunName, serverIP)
			_ = tunnel.SetupGlobalRoutes(e.cfg.TunName, serverIP)
		}
	}

	e.running = true
	loggo.Info("[Engine] YellowSocks Core Engine is fully UP and running!")
	return nil
}

func (e *Engine) cleanupServices() {
	if e.socks5Server != nil {
		_ = e.socks5Server.Stop()
	}
	if e.httpServer != nil {
		_ = e.httpServer.Stop()
	}
	if e.dnsServer != nil {
		_ = e.dnsServer.Stop()
	}
	if e.sppManager != nil {
		e.sppManager.Close()
	}
}

// Stop 停止引擎并恢复系统网络
func (e *Engine) Stop() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.running {
		return nil
	}
	e.running = false

	loggo.Info("[Engine] Shutting down YellowSocks Engine...")

	if !e.cfg.DisableTun && e.cfg.SetAutoRoute {
		sppHost, _, _ := net.SplitHostPort(e.cfg.SPPServer)
		if sppHost == "" {
			sppHost = e.cfg.SPPServer
		}
		sppIPs, _ := net.LookupIP(sppHost)
		var serverIP string
		if len(sppIPs) > 0 {
			serverIP = sppIPs[0].String()
		}
		_ = tunnel.RestoreGlobalRoutes(e.cfg.TunName, serverIP)
	}

	if e.stack != nil {
		e.stack.Close()
	}
	if e.socks5Server != nil {
		_ = e.socks5Server.Stop()
	}
	if e.httpServer != nil {
		_ = e.httpServer.Stop()
	}
	if e.dnsServer != nil {
		_ = e.dnsServer.Stop()
	}
	if e.sppManager != nil {
		e.sppManager.Close()
	}
	if e.router != nil {
		e.router.Close()
	}

	loggo.Info("[Engine] YellowSocks Engine stopped cleanly.")
	return nil
}

// SPPManager 暴露 SPP 节点管理器供 GUI/外部控制
func (e *Engine) SPPManager() *sppclient.Manager {
	return e.sppManager
}

// Socks5Server 暴露本地 SOCKS5 服务
func (e *Engine) Socks5Server() *proxy.Socks5Server {
	return e.socks5Server
}

// HTTPServer 暴露本地 HTTP 代理服务
func (e *Engine) HTTPServer() *proxy.HTTPServer {
	return e.httpServer
}

// DNSServer 暴露 DNS 服务
func (e *Engine) DNSServer() *appdns.Server {
	return e.dnsServer
}

// Router 暴露路由器
func (e *Engine) Router() *router.Router {
	return e.router
}
