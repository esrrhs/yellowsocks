package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/loggo"
	"github.com/getlantern/systray"
	"github.com/esrrhs/yellowsocks/core"
	"github.com/esrrhs/yellowsocks/core/sppclient"
	"github.com/esrrhs/yellowsocks/core/stats"
	"github.com/esrrhs/yellowsocks/core/sysproxy"
)

//go:embed web/index.html
var embeddedWebDashboard []byte

// AppConfig GUI 配置
type AppConfig struct {
	SPPServers   []*sppclient.Node `json:"nodes"`
	WintunName   string            `json:"wintun_name"`
	EnableFakeIP bool              `json:"enable_fake_ip"`
	BypassApps   []string          `json:"bypass_apps"`
	ChinaDNS     string            `json:"china_dns"`
	OverseasDoH  string            `json:"overseas_doh"`
	AutoRoute    bool              `json:"auto_route"`
	WebPort      int               `json:"web_port"`
}

var (
	engine   *core.Engine
	appCfg   AppConfig
	mRunning *systray.MenuItem
)

func main() {
	defer common.CrashLog()

	sppServer := flag.String("spp-server", "", "Default SPP remote server address")
	sppProto := flag.String("spp-proto", "tcp", "SPP protocol (tcp, udp, kcp, quic)")
	sppKey := flag.String("spp-key", "123456", "SPP password / key")
	sppEncrypt := flag.String("spp-encrypt", "default", "SPP encryption method")
	sppCompress := flag.Int("spp-compress", 128, "SPP compression threshold")
	wintunName := flag.String("tun-name", "YellowSocks", "Wintun adapter name")
	dohURL := flag.String("doh-url", "https://1.1.1.1/dns-query", "Overseas DoH endpoint")
	chinaDNS := flag.String("china-dns", "223.5.5.5:53", "China domestic DNS")
	fakeIP := flag.Bool("fake-ip", true, "Enable Fake-IP mode (Clash style)")
	autoRoute := flag.Bool("auto-route", true, "Automatically setup and restore Windows global routes")
	bypassApps := flag.String("bypass-apps", "dota2.exe,dota2,thunder.exe,xunlei.exe,baidunetdisk.exe,cloudmusic.exe", "Comma-separated process names to bypass")
	webPort := flag.Int("port", 9090, "Local dashboard HTTP API port")

	flag.Parse()

	loggo.Ini(loggo.Config{
		Level:  loggo.LEVEL_INFO,
		Prefix: "yellowsocks-gui",
	})

	loggo.Info("Starting YellowSocks Windows Tray & Dashboard GUI...")

	nodes := []*sppclient.Node{}
	if *sppServer != "" {
		nodes = append(nodes, &sppclient.Node{
			Name:        "Default-Server",
			Server:      *sppServer,
			ServerProto: *sppProto,
			Key:         *sppKey,
			Encrypt:     *sppEncrypt,
			Compress:    *sppCompress,
		})
	}

	var bypassList []string
	if *bypassApps != "" {
		for _, part := range strings.Split(*bypassApps, ",") {
			p := strings.TrimSpace(part)
			if p != "" {
				bypassList = append(bypassList, p)
			}
		}
	}

	appCfg = AppConfig{
		SPPServers:   nodes,
		WintunName:   *wintunName,
		EnableFakeIP: *fakeIP,
		BypassApps:   bypassList,
		ChinaDNS:     *chinaDNS,
		OverseasDoH:  *dohURL,
		AutoRoute:    *autoRoute,
		WebPort:      *webPort,
	}

	// 启动本地轻量 Dashboard API
	go startDashboardAPI()

	// 启动 Windows 真正原生窗口界面
	RunNativeWindow()
}

func startProxyEngine() error {
	if len(appCfg.SPPServers) == 0 {
		return fmt.Errorf("no SPP server configured")
	}
	cfg := core.EngineConfig{
		TunName:      appCfg.WintunName,
		TunIP:        "10.255.0.2",
		TunGateway:   "10.255.0.1",
		TunMask:      "255.255.255.0",
		MTU:          1500,
		SPPNodes:     appCfg.SPPServers,
		EnableFakeIP: appCfg.EnableFakeIP,
		BypassApps:   appCfg.BypassApps,
		ChinaDNS:     appCfg.ChinaDNS,
		OverseasDoH:  appCfg.OverseasDoH,
		SetAutoRoute: appCfg.AutoRoute,
	}
	engine = core.NewEngine(cfg)
	return engine.Start()
}

func stopProxyEngine() {
	if engine != nil {
		_ = engine.Stop()
		engine = nil
	}
}

func onReady() {
	systray.SetTitle("YellowSocks")
	systray.SetTooltip("YellowSocks (TUN & SPP Client)")

	mTitle := systray.AddMenuItem("YellowSocks Proxy: OFF", "Current Status")
	mTitle.Disable()

	systray.AddSeparator()
	mToggle := systray.AddMenuItem("Start TUN Proxy", "Toggle TUN Virtual Network Proxy")
	mSysProxy := systray.AddMenuItem("System Proxy: OFF", "Toggle Windows System Proxy (Internet Settings)")
	mFakeIP := systray.AddMenuItem("Fake-IP: ENABLED", "Toggle Fake-IP mode")
	mDashboard := systray.AddMenuItem(fmt.Sprintf("Open Web Dashboard (:%d)", appCfg.WebPort), "View dashboard")

	systray.AddSeparator()
	mNodes := systray.AddMenuItem("SPP Nodes", "Switch Active SPP Node")
	mQuit := systray.AddMenuItem("Quit", "Quit YellowSocks")

	mRunning = mTitle

	var isRunning bool
	var isSysProxyEnabled bool

	// 监听托盘交互
	go func() {
		for {
			select {
			case <-mToggle.ClickedCh:
				if !isRunning {
					if err := startProxyEngine(); err == nil {
						isRunning = true
						mTitle.SetTitle("YellowSocks Proxy: ACTIVE")
						mToggle.SetTitle("Stop TUN Proxy")
					} else {
						loggo.Error("Failed to start proxy: %v", err)
					}
				} else {
					stopProxyEngine()
					isRunning = false
					mTitle.SetTitle("YellowSocks Proxy: OFF")
					mToggle.SetTitle("Start TUN Proxy")
				}

			case <-mSysProxy.ClickedCh:
				if !isSysProxyEnabled {
					socksAddr := "127.0.0.1:10808"
					if engine != nil && engine.SPPManager() != nil {
						socksAddr = engine.SPPManager().Socks5Addr()
					}
					if err := sysproxy.SetGlobalProxy(socksAddr); err == nil {
						isSysProxyEnabled = true
						mSysProxy.SetTitle("System Proxy: ON")
						loggo.Info("[SysProxy] Windows system proxy enabled -> %s", socksAddr)
					}
				} else {
					_ = sysproxy.ClearSystemProxy()
					isSysProxyEnabled = false
					mSysProxy.SetTitle("System Proxy: OFF")
					loggo.Info("[SysProxy] Windows system proxy disabled")
				}

			case <-mFakeIP.ClickedCh:
				appCfg.EnableFakeIP = !appCfg.EnableFakeIP
				if appCfg.EnableFakeIP {
					mFakeIP.SetTitle("Fake-IP: ENABLED")
				} else {
					mFakeIP.SetTitle("Fake-IP: DISABLED")
				}

			case <-mDashboard.ClickedCh:
				loggo.Info("Web Dashboard available at http://127.0.0.1:%d", appCfg.WebPort)

			case <-mNodes.ClickedCh:
				loggo.Info("Manage nodes via Dashboard")

			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()

	// 监听 OS 信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		systray.Quit()
	}()
}

func onExit() {
	if engine != nil {
		_ = engine.Stop()
	}
	loggo.Info("YellowSocks GUI exited.")
}

func startDashboardAPI() {
	// 首页 Web Dashboard
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(embeddedWebDashboard)
	})

	// 实时流量、活跃连接与日志监控 API
	http.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		snapshot := stats.Default.GetSnapshot()
		_ = json.NewEncoder(w).Encode(snapshot)
	})

	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var activeNode *sppclient.Node
		if engine != nil && engine.SPPManager() != nil {
			activeNode = engine.SPPManager().ActiveNode()
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"running":     engine != nil,
			"fake_ip":     appCfg.EnableFakeIP,
			"active_node": activeNode,
			"nodes":       appCfg.SPPServers,
		})
	})

	http.HandleFunc("/api/switch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Index int `json:"index"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil && engine != nil {
			if mgr := engine.SPPManager(); mgr != nil {
				_ = mgr.SwitchToNode(req.Index)
			}
		}
		w.WriteHeader(http.StatusOK)
	})

	_ = http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", appCfg.WebPort), nil)
}
