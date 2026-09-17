package core

import (
	"fmt"
	"net"
	"sync"

	"github.com/esrrhs/gohome/loggo"
	tun2socksCore "github.com/xjasonlyu/tun2socks/v2/core"
	"github.com/xjasonlyu/tun2socks/v2/core/device/tun"
	gvisorStack "gvisor.dev/gvisor/pkg/tcpip/stack"
	appdns "github.com/esrrhs/yellowsocks/core/dns"
	"github.com/esrrhs/yellowsocks/core/router"
	"github.com/esrrhs/yellowsocks/core/sppclient"
	"github.com/esrrhs/yellowsocks/core/tunnel"
)

// EngineConfig 核心配置
type EngineConfig struct {
	TunName       string
	TunIP         string
	TunGateway    string
	TunMask       string
	MTU           int
	SPPServer     string // 远端 SPP 服务器 IP:Port (单节点向后兼容)
	SPPProto      string // SPP 协议类型 (tcp/udp/quic等)
	SPPKey        string
	SPPEncrypt    string
	SPPCompress   int
	SPPNodes      []*sppclient.Node // 多节点列表
	EnableFakeIP  bool              // 开启 Fake-IP 秒开模式
	BypassApps    []string          // 排除直连的应用名称列表 (TUN Bypass Apps)
	LocalSocks5   string            // 本地 SPP 暴露给内核的 socks5 端口
	DNSListen     string            // DNS listen address (default: 127.0.0.1:53)
	DirectDNS     string            // Direct resolver for local/domestic domain resolution
	RemoteDoH     string            // Remote DoH endpoint routed via SPP (default: https://1.1.1.1/dns-query)
	SetAutoRoute  bool              // Automatically manage system global routes
}

// Engine 统一网络引擎
type Engine struct {
	cfg        EngineConfig
	stack      *gvisorStack.Stack
	router     *router.Router
	dnsServer  *appdns.Server
	sppManager *sppclient.Manager
	mu         sync.Mutex
	running    bool
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

	loggo.Info("[Engine] Initializing Router (Bypass Apps: %v)...", e.cfg.BypassApps)
	e.router = router.NewRouter()
	for _, app := range e.cfg.BypassApps {
		e.router.AddBypassApp(app)
	}

	// 1. 整理 SPP 节点列表并启动 SPP 多节点故障转移管理器
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

	// 2. 启动本地 DNS 拦截服务 (支持 Fake-IP 秒开)
	loggo.Info("[Engine] Starting DNS Interceptor & Cache (Fake-IP: %v)...", e.cfg.EnableFakeIP)
	dnsSrv, err := appdns.NewServer(appdns.Config{
		ListenAddr:   e.cfg.DNSListen,
		DoHURL:       e.cfg.RemoteDoH,
		DirectDNS:    e.cfg.DirectDNS,
		Socks5Addr:   e.sppManager.Socks5Addr(),
		Router:       e.router,
		EnableFakeIP: e.cfg.EnableFakeIP,
	})
	if err != nil {
		e.sppManager.Close()
		return fmt.Errorf("failed to start DNS server: %w", err)
	}
	if err := dnsSrv.Start(); err != nil {
		e.sppManager.Close()
		return fmt.Errorf("failed to run DNS server: %w", err)
	}
	e.dnsServer = dnsSrv

	// 3. 打开 TUN 虚拟网卡 (纯 Go 实现，跨平台)
	loggo.Info("[Engine] Opening TUN virtual network device %s...", e.cfg.TunName)
	dev, err := tun.Open(e.cfg.TunName, uint32(e.cfg.MTU))
	if err != nil {
		_ = e.dnsServer.Stop()
		e.sppManager.Close()
		return fmt.Errorf("failed to open TUN device: %w", err)
	}

	// 4. 初始化 tun2socks v2 纯 Go 用户态 TCP/IP 协议栈
	handler := tunnel.NewHandler(e.router, e.dnsServer, e.sppManager)
	stack, err := tun2socksCore.CreateStack(&tun2socksCore.Config{
		LinkEndpoint:     dev,
		TransportHandler: handler,
	})
	if err != nil {
		dev.Close()
		_ = e.dnsServer.Stop()
		e.sppManager.Close()
		return fmt.Errorf("failed to create network stack: %w", err)
	}
	e.stack = stack

	// 5. 自动配置系统路由
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

	e.running = true
	loggo.Info("[Engine] YellowSocks Core Engine is fully UP and running!")
	return nil
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
		_ = tunnel.RestoreGlobalRoutes(e.cfg.TunName, serverIP)
	}

	if e.stack != nil {
		e.stack.Close()
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
