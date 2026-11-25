package guiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/yellowsocks/core"
	"github.com/esrrhs/yellowsocks/core/config"
	"github.com/esrrhs/yellowsocks/core/router"
	"github.com/esrrhs/yellowsocks/core/stats"
	"github.com/esrrhs/yellowsocks/core/version"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const configName = "config.yaml"
const settingsName = "settings.json"
const statsName = "stats.json"

// guiSettings is the GUI-only state that does not belong in the CLI config
// file, persisted next to it.
type guiSettings struct {
	SystemProxy   bool   `json:"system_proxy"`
	AutoStartCore bool   `json:"auto_start_core"`
	LaunchAtLogin bool   `json:"launch_at_login"`
	TunMode       bool   `json:"tun_mode"`
	// ProxyMode is one of "rule" (default), "global" or "direct". Stored as a
	// string rather than an enum so an older build reading a newer file keeps
	// the value instead of dropping it.
	ProxyMode string `json:"proxy_mode,omitempty"`
	// ProxyGuard keeps the OS proxy registration pointed at us. Opt-in: while
	// it is on, switching the system proxy off from Windows is undone within
	// half a minute.
	ProxyGuard   bool   `json:"proxy_guard,omitempty"`
	AppliedPorts string `json:"sysproxy_applied_ports,omitempty"`
}

// defaultConfig is written on first launch. It uses the same format as the
// CLI config file, so the two can be exchanged freely.
const defaultConfig = `# YellowSocks 配置(与 CLI 的 config.yaml 格式完全相同)
# 修改后点击设置页的"保存",再启动引擎即可生效。

# 上游 SPP 服务器
spp_server: "your_spp_server:8888"
spp_proto: "tcp"
spp_key: "123456"

# 本地监听
# 注意: 引擎总是启动 DNS 服务,macOS 上 53 端口需要管理员,默认用 1053
dns_listen: "127.0.0.1:1053"
socks5_listen: "127.0.0.1:10808"
http_listen: "127.0.0.1:8080"

loglevel: "info"
`

// EngineStatus is the engine state pushed to the frontend via the
// "engine:status" event and returned by EngineStatus().
type EngineStatus struct {
	Running     bool   `json:"running"`
	SystemProxy bool   `json:"system_proxy"`
	Tun         bool   `json:"tun"`
	TunError    string `json:"tun_error"`
	Socks5      string `json:"socks5"`
	HTTP        string `json:"http"`
	DNS         string `json:"dns"`
	SPPServer   string `json:"spp_server"`
	SPPProto    string `json:"spp_proto"`
	ProxyMode   string `json:"proxy_mode"`
	Error       string `json:"error"`
}

// App is the structure bound to the Wails frontend. Every exported method
// becomes callable from JavaScript (see frontend/wailsjs).
type App struct {
	ctx context.Context

	mu             sync.Mutex
	engine         *core.Engine
	lastCfg        core.EngineConfig
	running        bool
	quitting       bool
	sysProxyWanted bool   // user intent, persisted in settings.json
	sysProxyActive bool   // we registered the OS proxy and must undo it
	autoStartCore  bool   // start the engine when the app launches
	launchAtLogin  bool   // register the app to start with the OS login
	tunWanted      bool   // TUN mode intent, persisted in settings.json
	tunActive      bool   // the virtual adapter is up and capturing traffic
	tunErr         string // last TUN failure, surfaced in the UI
	proxyMode      string // "rule" | "global" | "direct", persisted in settings.json
	proxyGuard     bool   // keep the OS proxy registration ours, persisted too
	appliedPorts   string // ports registered with the OS, for stale cleanup

	// tunApplyMu serializes bring-up and teardown of the adapter. Two paths
	// race for it: StartEngine asynchronously restores the persisted TUN
	// intent, while the user (or a test driver) can toggle the switch at the
	// same instant. Unsynchronized, the loser's teardown lands right after
	// the winner's bring-up and the adapter flashes up and vanishes.
	tunApplyMu sync.Mutex

	// frontendEvents enables Wails runtime calls (EventsEmit). The Wails
	// shells turn it on; the test driver leaves it off — the runtime exits
	// the process on any call with a non-lifecycle context.
	frontendEvents bool

	// Last known permanent traffic totals. Refreshed from the live
	// collector while the engine runs; seeded from the stats file at
	// startup so the record is visible before the engine starts.
	lifetimeAll    stats.Counters
	lifetimeDirect stats.Counters
	lifetimeProxy  stats.Counters
}

// NewApp creates the App application struct that is bound to the frontend.
func NewApp() *App {
	// Rule-based routing is the default, and it has to be set here rather than
	// left empty: the tray and the window read this before Startup has loaded
	// the settings file, and an empty mode would render as "no mode selected".
	return &App{proxyMode: router.ModeRule.String()}
}

// startup is called when the app starts. The context is saved so we can call
// the Wails runtime, then the tray is started in its own goroutine and the
// engine may auto-start per the persisted settings.
func (a *App) Startup(ctx context.Context) {
	a.ctx = ctx
	a.mu.Lock()
	var st guiSettings
	if s, err := loadSettings(); err == nil {
		st = s
		a.sysProxyWanted = st.SystemProxy
		a.autoStartCore = st.AutoStartCore
		a.launchAtLogin = st.LaunchAtLogin
		a.tunWanted = st.TunMode
		a.proxyMode = router.ParseMode(st.ProxyMode).String()
		a.proxyGuard = st.ProxyGuard
	}
	a.mu.Unlock()
	// Keep the OS launch-at-login entry pointing at the current exe (the
	// binary may have been moved since it was registered). Off the startup
	// path: repairing the logon task costs a PowerShell launch, and nothing
	// waits for it.
	go syncLaunchAtLogin(st)
	a.loadLifetimeStats()
	go runTray(a)
	go a.proxyGuardLoop()
	// TUN mode needs the engine running; the boot flag alone is not enough.
	wantEngine := st.AutoStartCore || st.TunMode
	if wantEngine {
		go a.startEngineAndTun()
	}
}

// startEngineAndTun starts the engine; TUN mode rides on StartEngine's
// tail (the engine autostarts whenever AutoStartCore or TunMode wants it).
func (a *App) startEngineAndTun() {
	if ElevatedRequested() && !ElevatedDirect() {
		// The unelevated instance is still tearing down while this copy
		// boots; give its listen ports time to close. The logon task starts
		// us on a clean machine, so there is nothing to wait for there.
		if cfg, err := a.loadEngineConfig(); err == nil {
			WaitForPortFree(cfg.Socks5Listen, 10*time.Second)
			WaitForPortFree(cfg.HTTPListen, 5*time.Second)
		}
	}
	_ = a.StartEngine()
}

// beforeClose intercepts the window close event. Instead of quitting, the
// window hides to the tray; real quits go through the tray menu, which sets
// quitting first.
func (a *App) BeforeClose(ctx context.Context) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.quitting {
		return false
	}
	runtime.WindowHide(ctx)
	return true
}

// shutdown is called on real application exit. It tears the TUN adapter
// down first (its routes capture traffic even without the engine), stops
// the engine, removes the tray icon and undoes the system proxy.
func (a *App) Shutdown(ctx context.Context) {
	a.mu.Lock()
	a.quitting = true
	running := a.running
	active := a.sysProxyActive
	a.mu.Unlock()
	a.tunApplyMu.Lock()
	// Process exit: unlike a TUN toggle (which only removes the capture
	// point and keeps the packet stack warm), this releases the adapter and
	// stops the stack for good.
	_ = tunShutdown()
	a.tunApplyMu.Unlock()
	if running {
		_ = a.StopEngine()
	}
	if active {
		_ = disableSystemProxy()
	}
	systrayQuit()
}

// StartEngine loads the config file and starts the proxy engine.
func (a *App) StartEngine() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return errors.New("engine already running")
	}
	cfg, err := a.loadEngineConfig()
	if err != nil {
		a.emitStatusLocked(err)
		return err
	}
	engine := core.NewEngine(cfg)
	// Apply the remembered policy before Start: the router is created inside
	// Start, and the engine carries the mode over to it.
	engine.SetRoutingMode(router.ParseMode(a.proxyMode))
	if err := engine.Start(); err != nil {
		a.emitStatusLocked(err)
		return err
	}
	a.engine = engine
	a.lastCfg = cfg
	a.running = true
	a.emitStatusLocked(nil)
	loggo.Info("[GUI] engine started")

	// Restore the system proxy registration if the user wants it on.
	if a.sysProxyWanted && !a.sysProxyActive {
		if err := a.applySystemProxyLocked(true); err != nil {
			loggo.Warn("[GUI] enable system proxy: %v", err.Error())
		}
	}

	// TUN mode rides on a running engine: bring the adapter up whenever the
	// engine starts with the intent on (boot autostart, manual restart, or
	// the elevated relaunch). applyTun is a no-op if it is already up.
	if a.tunWanted {
		go func() { _ = a.applyTun() }()
	}
	return nil
}

// StopEngine stops the proxy engine. Safe to call when it is not running.
func (a *App) StopEngine() error {
	// The TUN stack forwards into the engine inbound and holds references
	// to its addresses: always bring it down before the engine.
	a.tunApplyMu.Lock()
	_ = tunDisable()
	a.disableTUNDNS()
	a.tunApplyMu.Unlock()
	a.mu.Lock()
	a.tunActive = false
	defer a.mu.Unlock()
	if !a.running {
		return nil
	}
	if a.sysProxyActive {
		if err := a.applySystemProxyLocked(false); err != nil {
			loggo.Warn("[GUI] disable system proxy: %s", err.Error())
		}
	}
	err := a.engine.Stop()
	a.engine = nil
	a.running = false
	a.emitStatusLocked(err)
	loggo.Info("[GUI] engine stopped")
	return err
}

// GetSystemProxy reports whether the GUI should keep the system proxy
// registered while the engine runs.
func (a *App) GetSystemProxy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sysProxyWanted
}

// SetSystemProxy toggles the system proxy. The intent is persisted; it takes
// effect immediately when the engine is running, otherwise on next start.
func (a *App) SetSystemProxy(on bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sysProxyWanted = on
	if err := a.persistSettingsLocked(); err != nil {
		loggo.Warn("[GUI] persist settings: %s", err.Error())
	}
	if a.running {
		return a.applySystemProxyLocked(on)
	}
	return nil
}

// GetTunMode reports the TUN intent (the adapter may still be coming up).
func (a *App) GetTunMode() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tunWanted
}

// GetProxyMode reports the routing policy: "rule" (default), "global" or
// "direct".
func (a *App) GetProxyMode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.proxyMode
}

// GetProxyGuard reports whether the system proxy registration is being
// defended against changes made by other programs.
func (a *App) GetProxyGuard() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.proxyGuard
}

// SetProxyGuard turns the guard on or off and persists the choice. Turning it
// on while the system proxy is registered repairs the registration right away,
// so the first check is not half a minute late.
func (a *App) SetProxyGuard(on bool) error {
	a.mu.Lock()
	a.proxyGuard = on
	err := a.persistSettingsLocked()
	a.mu.Unlock()
	if err != nil {
		loggo.Warn("[GUI] persist settings: %s", err.Error())
	}
	a.emitStatus()
	if on {
		// Repair immediately instead of waiting for the next tick: turning the
		// switch on means "make this true now".
		go a.proxyGuardCheck()
	}
	return err
}

// SetProxyMode switches the routing policy and persists it. It takes effect on
// the running engine immediately (the router reads the mode from an atomic,
// so in-flight connections are not interrupted) and is re-applied on the next
// Start, which builds a fresh router.
func (a *App) SetProxyMode(mode string) error {
	m := router.ParseMode(mode)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.proxyMode = m.String()
	if err := a.persistSettingsLocked(); err != nil {
		loggo.Warn("[GUI] persist settings: %s", err.Error())
	}
	if a.running && a.engine != nil {
		a.engine.SetRoutingMode(m)
	}
	loggo.Info("[GUI] routing mode -> %s", a.proxyMode)
	a.emitStatusLocked(nil)
	return nil
}

// TunSessionStats returns live TUN packet counters (diagnostics).
func (a *App) TunSessionStats() string { return tunSessionStatsLive() }

// SetTunMode toggles TUN mode. Enabling needs the engine running and
// administrator rights: from an unelevated session the app persists the
// intent and relaunches itself elevated (one UAC prompt); a declined UAC
// keeps the intent off and reports the failure. Disabling tears the adapter
// down immediately. Enabling turns the redundant system proxy off; disabling
// restores it from its saved intent.
func (a *App) SetTunMode(on bool) error {
	a.mu.Lock()
	a.tunWanted = on
	persistErr := a.persistSettingsLocked()
	a.mu.Unlock()
	if persistErr != nil {
		loggo.Warn("[GUI] persist settings: %s", persistErr.Error())
	}

	if !on {
		a.tunApplyMu.Lock()
		err := tunDisable()
		a.disableTUNDNS()
		a.tunApplyMu.Unlock()
		a.mu.Lock()
		a.tunActive = false
		a.tunErr = ""
		a.mu.Unlock()
		// Restore the system proxy from its saved intent.
		a.mu.Lock()
		wanted, running := a.sysProxyWanted, a.running
		a.mu.Unlock()
		if wanted && running {
			if err := a.SetSystemProxy(true); err != nil {
				loggo.Warn("[GUI] restore system proxy after TUN off: %s", err.Error())
			}
		}
		a.emitStatus()
		return err
	}
	return a.applyTun()
}

// applyTun brings the virtual adapter up. Must not hold a.mu: the elevated
// relaunch quits the process and tunEnable has its own lock.
func (a *App) applyTun() error {
	a.tunApplyMu.Lock()
	defer a.tunApplyMu.Unlock()
	if tunIsActive() {
		return nil
	}
	a.mu.Lock()
	running, cfg := a.running, a.lastCfg
	a.mu.Unlock()
	if !running {
		return errors.New("TUN 模式需要引擎处于运行状态,请先启动代理")
	}

	if !isElevated() {
		// Persisted already; hand over to an elevated copy of ourselves and
		// wait for it to prove it started (the user might decline the UAC
		// prompt — this instance must stay alive in that case).
		ClearElevatedHandshake()
		if err := relaunchElevated(); err != nil {
			a.mu.Lock()
			a.tunWanted = false
			_ = a.persistSettingsLocked()
			a.tunErr = err.Error()
			a.mu.Unlock()
			a.emitStatus()
			return err
		}
		if WaitElevatedHandshake(30 * time.Second) {
			a.mu.Lock()
			a.tunErr = ""
			a.mu.Unlock()
			loggo.Info("[GUI] elevated instance confirmed; handing over")
			// Give the graceful shutdown a moment, then force it: the
			// elevated successor re-binds the same ports and keeps the
			// system proxy (or TUN) state, so nothing is left dangling.
			go func() {
				time.Sleep(3 * time.Second)
				loggo.Info("[GUI] graceful shutdown timed out during handover; exiting")
				os.Exit(0)
			}()
			runtime.Quit(a.ctx)
			return nil
		}
		handoverErr := errors.New("管理员授权未完成,TUN 模式未开启")
		a.mu.Lock()
		a.tunWanted = false
		_ = a.persistSettingsLocked()
		a.tunErr = handoverErr.Error()
		a.mu.Unlock()
		a.emitStatus()
		return handoverErr
	}

	params := a.tunParamsFrom(cfg)
	params.Soak = tunWatchdogSoak
	loggo.Info("[GUI] applyTun: tunWatchdogSoak=%v active-before=%v", tunWatchdogSoak, tunIsActive())
	if err := tunEnable(params); err != nil {
		a.disableTUNDNS()
		a.mu.Lock()
		a.tunActive = false
		a.tunErr = err.Error()
		a.mu.Unlock()
		a.emitStatus()
		return err
	}
	// The adapter is up: flip the engine into TUN-mode DNS handling so every
	// captured :53 query is answered in-process (smart split + fake-ip) and
	// proxied domains reach the upstream by name instead of as real IPs.
	a.enableTUNDNS()
	a.mu.Lock()
	a.tunActive = true
	a.tunErr = ""
	// The TUN captures everything; the WinINET proxy would only double-apply.
	sysProxyOn := a.sysProxyWanted
	a.mu.Unlock()
	if sysProxyOn {
		_ = a.SetSystemProxy(false)
	}
	a.emitStatus()
	loggo.Info("[GUI] TUN mode enabled")
	return nil
}

// enableTUNDNS flips the running engine's DNS server into TUN-mode handling
// (in-process :53 hijack + fake-ip). A no-op when the engine is not running.
func (a *App) enableTUNDNS() {
	a.mu.Lock()
	e := a.engine
	a.mu.Unlock()
	if e == nil {
		return
	}
	if dns := e.DNS(); dns != nil {
		if err := dns.EnableTUN(""); err != nil {
			loggo.Warn("[GUI] enable TUN DNS: %v", err)
		}
	}
}

// disableTUNDNS reverts enableTUNDNS. Safe to call when the engine is gone.
func (a *App) disableTUNDNS() {
	a.mu.Lock()
	e := a.engine
	a.mu.Unlock()
	if e == nil {
		return
	}
	if dns := e.DNS(); dns != nil {
		dns.DisableTUN()
	}
}

// tunParamsFrom collects the engine endpoints the TUN stack forwards into
// plus the upstream IPs that need physical-gateway escape routes.
func (a *App) tunParamsFrom(cfg core.EngineConfig) TunParams {
	params := TunParams{
		Socks5Addr: cfg.Socks5Listen,
		DNSAddr:    cfg.DNSListen,
	}
	if params.Socks5Addr == "" {
		params.Socks5Addr = "127.0.0.1:10808"
	}

	hosts := map[string]struct{}{}
	addHost := func(server string) {
		host, _, err := net.SplitHostPort(server)
		if err != nil || host == "" {
			host = server
		}
		if host != "" {
			hosts[host] = struct{}{}
		}
	}
	addHost(cfg.SPPServer)
	for _, node := range cfg.SPPNodes {
		addHost(node.Server)
	}
	for host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			if !ip.IsLoopback() {
				params.UpstreamIPs = append(params.UpstreamIPs, ip.String())
			}
			continue
		}
		// A hostname upstream resolves now, while the physical adapter is
		// still the default route.
		ips, err := net.LookupHost(host)
		if err != nil {
			loggo.Warn("[GUI] resolve upstream %s for TUN routes: %v", host, err)
			continue
		}
		for _, ipStr := range ips {
			if ip := net.ParseIP(ipStr); ip != nil && !ip.IsLoopback() {
				params.UpstreamIPs = append(params.UpstreamIPs, ipStr)
			}
		}
	}
	// The TUN watchdog auto-rolls-back a dead pipeline so the user's network
	// recovers on its own; surface the reason in the UI.
	params.OnRollback = func(reason string) {
		a.disableTUNDNS()
		a.mu.Lock()
		a.tunActive = false
		a.tunErr = "TUN 管线自检失败,已自动回滚: " + reason
		a.mu.Unlock()
		a.emitStatus()
	}
	return params
}

// GetAutoStartCore reports whether the engine auto-starts with the app.
func (a *App) GetAutoStartCore() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.autoStartCore
}

// SetAutoStartCore toggles engine auto-start. Takes effect on next launch.
func (a *App) SetAutoStartCore(on bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.autoStartCore = on
	return a.persistSettingsLocked()
}

// GetLaunchAtLogin reports whether the app is registered to start with the
// OS login. The OS registry is the source of truth.
func (a *App) GetLaunchAtLogin() bool {
	return launchAtLoginEnabled()
}

// SetLaunchAtLogin registers or removes the launch-at-login entry. On Windows
// it is a per-user Run-key entry that starts the app hidden in the tray;
// together with 自动连接 and 系统代理 this gives the full boot-time transparent
// proxy. The intent is persisted so a moved exe can be re-registered.
func (a *App) SetLaunchAtLogin(on bool) error {
	if on {
		if err := enableLaunchAtLogin(); err != nil {
			return err
		}
	} else if err := disableLaunchAtLogin(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.launchAtLogin = on
	return a.persistSettingsLocked()
}

// persistSettingsLocked writes the current GUI settings to disk. Callers
// must hold a.mu.
func (a *App) persistSettingsLocked() error {
	return saveSettings(guiSettings{
		SystemProxy:   a.sysProxyWanted,
		AutoStartCore: a.autoStartCore,
		LaunchAtLogin: a.launchAtLogin,
		TunMode:       a.tunWanted,
		AppliedPorts:  a.appliedPorts,
	})
}

// applySystemProxyLocked registers or removes the OS-wide proxy using the
// engine's inbound ports. Callers must hold a.mu.
func (a *App) applySystemProxyLocked(on bool) error {
	if on {
		if err := enableSystemProxy(a.lastCfg.HTTPListen, a.lastCfg.Socks5Listen); err != nil {
			return err
		}
		a.sysProxyActive = true
		a.appliedPorts = appliedPortsString(a.lastCfg.HTTPListen, a.lastCfg.Socks5Listen)
	} else {
		if err := disableSystemProxy(); err != nil {
			return err
		}
		a.sysProxyActive = false
		a.appliedPorts = ""
	}
	if err := a.persistSettingsLocked(); err != nil {
		loggo.Warn("[GUI] persist settings: %s", err.Error())
	}
	a.emitStatusLocked(nil)
	return nil
}

// appliedPortsString renders the ports we registered as "http;socks" for the
// stale-cleanup check on next launch.
func appliedPortsString(httpAddr, socksAddr string) string {
	httpPort, socksPort, err := proxyPorts(httpAddr, socksAddr)
	if err != nil {
		return ""
	}
	return strconv.Itoa(httpPort) + ";" + strconv.Itoa(socksPort)
}

// EngineStatus returns the current engine state.
func (a *App) EngineStatus() EngineStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.statusLocked()
}

// quitApp is invoked from the tray menu: stop the engine, then ask Wails to
// exit. App.shutdown removes the tray icon afterwards.
func (a *App) quitApp() {
	a.mu.Lock()
	a.quitting = true
	a.mu.Unlock()
	_ = a.StopEngine()
	runtime.Quit(a.ctx)
}

// GetOverview returns live traffic totals and rates, or nil when the engine
// is stopped. It also refreshes the cached lifetime totals.
func (a *App) GetOverview() *stats.Overview {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running || a.engine == nil {
		return nil
	}
	o := a.engine.Stats().Overview()
	a.lifetimeAll = o.Lifetime
	a.lifetimeDirect = o.LifetimeDirect
	a.lifetimeProxy = o.LifetimeProxy
	return &o
}

// loadLifetimeStats seeds the lifetime cache from the persisted stats file so
// the permanent record is visible before the engine starts.
func (a *App) loadLifetimeStats() {
	all, direct, proxy := stats.LifetimeTotals(statsPath())
	a.mu.Lock()
	a.lifetimeAll, a.lifetimeDirect, a.lifetimeProxy = all, direct, proxy
	a.mu.Unlock()
}

// GetLifetimeStats returns the permanent traffic record with its domestic
// (direct) and foreign (proxy) shares. The counters survive restarts and
// are never cleared except by ResetLifetimeStats.
func (a *App) GetLifetimeStats() stats.LifetimeStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return stats.LifetimeStats{
		Totals: a.lifetimeAll,
		Direct: a.lifetimeDirect,
		Proxy:  a.lifetimeProxy,
	}
}

// ResetLifetimeStats clears the permanent traffic record. While the engine is
// stopped the stats file is rewritten directly, so the cleared totals are
// not resurrected by the next launch.
func (a *App) ResetLifetimeStats() error {
	a.mu.Lock()
	if a.running && a.engine != nil {
		a.engine.Stats().LifetimeReset()
		a.lifetimeAll, a.lifetimeDirect, a.lifetimeProxy = stats.Counters{}, stats.Counters{}, stats.Counters{}
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()

	c := stats.New(stats.Config{File: statsPath(), SaveInterval: 0})
	c.LifetimeReset()
	err := c.Save()
	c.Close()
	a.mu.Lock()
	a.lifetimeAll, a.lifetimeDirect, a.lifetimeProxy = stats.Counters{}, stats.Counters{}, stats.Counters{}
	a.mu.Unlock()
	return err
}

// GetSeries returns the traffic time series for the last spanSeconds, sampled
// into roughly 60 buckets.
func (a *App) GetSeries(spanSeconds int) []stats.SeriesPoint {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running || a.engine == nil {
		return nil
	}
	if spanSeconds <= 0 {
		spanSeconds = 1800
	}
	step := time.Duration(spanSeconds/60) * time.Second
	if step < time.Second {
		step = time.Second
	}
	now := time.Now()
	return a.engine.Stats().Series(now.Add(-time.Duration(spanSeconds)*time.Second), step)
}

// GetSessions returns the currently active connections.
func (a *App) GetSessions() []stats.SessionView {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running || a.engine == nil {
		return nil
	}
	return a.engine.Stats().ActiveSessions(stats.ActiveOptions{Limit: 200})
}

// GetNodes returns per-upstream-node traffic and health.
func (a *App) GetNodes() []stats.NodeStat {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running || a.engine == nil {
		return nil
	}
	return a.engine.Stats().Nodes()
}

// GetErrors returns the recent error log.
func (a *App) GetErrors() []stats.ErrorEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running || a.engine == nil {
		return nil
	}
	return a.engine.Stats().Errors(100)
}

// GetConfig returns the current config file content, creating the default one
// on first launch.
func (a *App) GetConfig() (string, error) {
	path, err := configPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err := writeDefaultConfig(path); err != nil {
			return "", err
		}
		return defaultConfig, nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// SaveConfig validates and persists the config file. Validation reuses the
// CLI parser, so a saved GUI config is always loadable by the CLI and
// vice versa.
func (a *App) SaveConfig(content string) error {
	if _, err := config.ParseConfigContent([]byte(content)); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0644)
}

// GetVersion returns build information.
func (a *App) GetVersion() version.Info {
	return version.GetInfo()
}

// configPath is the config file location: next to the exe in portable mode,
// otherwise the per-user directory (e.g. %APPDATA%/YellowSocks).
func configPath() (string, error) {
	return filepath.Join(DataDir(), configName), nil
}

func writeDefaultConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(defaultConfig), 0644)
}

// settingsPath lives next to config.yaml so users can find both together.
func settingsPath() (string, error) {
	return filepath.Join(DataDir(), settingsName), nil
}

// statsPath is where the engine's traffic statistics are persisted. It lives
// with the other data files so the totals survive restarts and move with a
// portable exe.
func statsPath() string {
	return filepath.Join(DataDir(), statsName)
}

func loadSettings() (guiSettings, error) {
	path, err := settingsPath()
	if err != nil {
		return guiSettings{}, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return guiSettings{}, nil
	}
	if err != nil {
		return guiSettings{}, err
	}
	var st guiSettings
	if err := json.Unmarshal(data, &st); err != nil {
		return guiSettings{}, err
	}
	return st, nil
}

func saveSettings(st guiSettings) error {
	path, err := settingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// loadEngineConfig reads the config file and merges it onto safe defaults.
func (a *App) loadEngineConfig() (core.EngineConfig, error) {
	path, err := configPath()
	if err != nil {
		return core.EngineConfig{}, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		data = []byte(defaultConfig)
	} else if err != nil {
		return core.EngineConfig{}, err
	}
	fileCfg, err := config.ParseConfigContent(data)
	if err != nil {
		return core.EngineConfig{}, fmt.Errorf("invalid config: %w", err)
	}
	// The engine always starts DNS, so default to a non-privileged port:
	// 53 needs admin on macOS. Values in the config file override this.
	// Stats persist by default (next to the other data files) so the
	// lifetime traffic totals are kept forever; a stats_file in the config
	// file overrides this location.
	base := core.EngineConfig{
		DNSListen: "127.0.0.1:1053",
		StatsFile: statsPath(),
	}
	return fileCfg.MergeWithEngineConfig(base), nil
}

func (a *App) statusLocked() EngineStatus {
	st := EngineStatus{
		Running:     a.running,
		SystemProxy: a.sysProxyActive || a.sysProxyWanted,
		Tun:         a.tunActive,
		TunError:    a.tunErr,
		ProxyMode:   a.proxyMode,
		Error:       "",
	}
	if a.running {
		st.Socks5 = a.lastCfg.Socks5Listen
		st.HTTP = a.lastCfg.HTTPListen
		st.DNS = a.lastCfg.DNSListen
		st.SPPServer = a.lastCfg.SPPServer
		st.SPPProto = a.lastCfg.SPPProto
	}
	return st
}

// emitStatus pushes the current state to the frontend without an error to
// attach. For callers that do not hold a.mu.
func (a *App) emitStatus() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.emitStatusLocked(nil)
}

// EnableFrontendEvents turns on the Wails event plumbing. The GUI shells
// call it once at startup; tests and the driver must not: the Wails runtime
// hard-exits the process on any call without its lifecycle context.
func (a *App) EnableFrontendEvents() {
	a.mu.Lock()
	a.frontendEvents = true
	a.mu.Unlock()
}

// emitStatusLocked pushes the current state to the frontend. Callers must
// hold a.mu since it reads engine state.
func (a *App) emitStatusLocked(err error) {
	st := a.statusLocked()
	if err != nil {
		st.Error = err.Error()
	}
	if !a.frontendEvents {
		return
	}
	runtime.EventsEmit(a.ctx, "engine:status", st)
}
