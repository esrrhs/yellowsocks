package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/loggo"
	"github.com/getlantern/systray"
	"github.com/esrrhs/yellowsocks/core"
	"github.com/esrrhs/yellowsocks/core/config"
	"github.com/esrrhs/yellowsocks/core/sppclient"
	"github.com/esrrhs/yellowsocks/core/stats"
	"github.com/esrrhs/yellowsocks/core/sysproxy"
)

//go:embed web/index.html
var embeddedWebDashboard []byte

// AppConfig holds GUI application configuration
type AppConfig struct {
	SPPServers   []*sppclient.Node `json:"nodes"`
	WintunName   string            `json:"wintun_name"`
	EnableFakeIP bool              `json:"enable_fake_ip"`
	BypassApps   []string          `json:"bypass_apps"`
	DirectDNS    string            `json:"direct_dns"`
	RemoteDoH    string            `json:"remote_doh"`
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

	configPath := flag.String("config", "", "Path to json/yaml configuration file")
	sppServer := flag.String("spp-server", "", "Default SPP remote server address")
	sppProto := flag.String("spp-proto", "tcp", "SPP protocol (tcp, udp, kcp, quic)")
	sppKey := flag.String("spp-key", "123456", "SPP password / key")

	flag.Parse()

	var fileCfg *config.FileConfig
	if *configPath != "" {
		var err error
		fileCfg, err = config.LoadConfigFile(*configPath)
		if err != nil {
			fmt.Printf("Error loading config file: %v\n\n", err)
		}
	}

	level := loggo.LEVEL_INFO
	if fileCfg != nil && fileCfg.LogLevel != "" && loggo.NameToLevel(fileCfg.LogLevel) >= 0 {
		level = loggo.NameToLevel(fileCfg.LogLevel)
	}
	loggo.Ini(loggo.Config{
		Level:  level,
		Prefix: "yellowsocks-gui",
	})

	loggo.Info("Starting YellowSocks Tray & Dashboard GUI...")

	nodes := []*sppclient.Node{}
	if *sppServer != "" {
		nodes = append(nodes, &sppclient.Node{
			Name:        "Default-Server",
			Server:      *sppServer,
			ServerProto: *sppProto,
			Key:         *sppKey,
			Encrypt:     "default",
			Compress:    128,
		})
	}

	appCfg = AppConfig{
		SPPServers:   nodes,
		WintunName:   "YellowSocks",
		EnableFakeIP: true,
		DirectDNS:    "1.1.1.1:53",
		RemoteDoH:    "https://1.1.1.1/dns-query",
		AutoRoute:    true,
		WebPort:      9090,
	}

	if fileCfg != nil {
		if len(fileCfg.SPPNodes) > 0 {
			appCfg.SPPServers = fileCfg.SPPNodes
		} else if fileCfg.SPPServer != "" {
			proto := "tcp"
			if fileCfg.SPPProto != "" {
				proto = fileCfg.SPPProto
			}
			compress := 128
			if fileCfg.SPPCompress != nil {
				compress = *fileCfg.SPPCompress
			}
			encrypt := "default"
			if fileCfg.SPPEncrypt != "" {
				encrypt = fileCfg.SPPEncrypt
			}
			appCfg.SPPServers = []*sppclient.Node{
				{
					Name:        "Default-Server",
					Server:      fileCfg.SPPServer,
					ServerProto: proto,
					Key:         fileCfg.SPPKey,
					Encrypt:     encrypt,
					Compress:    compress,
				},
			}
		}
		if fileCfg.TunName != "" {
			appCfg.WintunName = fileCfg.TunName
		}
		if fileCfg.EnableFakeIP != nil {
			appCfg.EnableFakeIP = *fileCfg.EnableFakeIP
		}
		if len(fileCfg.BypassApps) > 0 {
			appCfg.BypassApps = fileCfg.BypassApps
		}
		if fileCfg.DirectDNS != "" {
			appCfg.DirectDNS = fileCfg.DirectDNS
		}
		if fileCfg.RemoteDoH != "" {
			appCfg.RemoteDoH = fileCfg.RemoteDoH
		}
		if fileCfg.AutoRoute != nil {
			appCfg.AutoRoute = *fileCfg.AutoRoute
		}
		if fileCfg.WebPort != nil {
			appCfg.WebPort = *fileCfg.WebPort
		}
	}

	// 启动本地轻量 Dashboard API
	go startDashboardAPI()

	// Start the native window / tray (platform-specific)
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
		DirectDNS:    appCfg.DirectDNS,
		RemoteDoH:    appCfg.RemoteDoH,
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

	// Handle tray menu interactions
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
						loggo.Info("[SysProxy] System proxy enabled -> %s", socksAddr)
					}
				} else {
					_ = sysproxy.ClearSystemProxy()
					isSysProxyEnabled = false
					mSysProxy.SetTitle("System Proxy: OFF")
					loggo.Info("[SysProxy] System proxy disabled")
				}

			case <-mFakeIP.ClickedCh:
				appCfg.EnableFakeIP = !appCfg.EnableFakeIP
				if appCfg.EnableFakeIP {
					mFakeIP.SetTitle("Fake-IP: ENABLED")
				} else {
					mFakeIP.SetTitle("Fake-IP: DISABLED")
				}

			case <-mDashboard.ClickedCh:
				url := fmt.Sprintf("http://127.0.0.1:%d", appCfg.WebPort)
				openBrowser(url)
				loggo.Info("Web Dashboard: %s", url)

			case <-mNodes.ClickedCh:
				loggo.Info("Manage nodes via Dashboard")

			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()

	// Listen for OS signals
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

// openBrowser opens the given URL in the user's default web browser.
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{url}
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start", url}
	default: // linux and others
		cmd = "xdg-open"
		args = []string{url}
	}
	_ = exec.Command(cmd, args...).Start()
}

func startDashboardAPI() {
	// Serve the embedded web dashboard
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(embeddedWebDashboard)
	})

	// Real-time traffic, connections and log API
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

	http.HandleFunc("/api/toggle", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var isRunning bool
		if engine == nil {
			isRunning = startProxyEngine() == nil
		} else {
			stopProxyEngine()
			isRunning = false
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"running": isRunning})
	})

	_ = http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", appCfg.WebPort), nil)
}
