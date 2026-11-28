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
	"sync/atomic"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/yellowsocks/core"
	"github.com/esrrhs/yellowsocks/core/config"
	"github.com/esrrhs/yellowsocks/core/router"
	"github.com/esrrhs/yellowsocks/core/sppclient"
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
	SystemProxy   bool `json:"system_proxy"`
	AutoStartCore bool `json:"auto_start_core"`
	LaunchAtLogin bool `json:"launch_at_login"`
	TunMode       bool `json:"tun_mode"`
	// ProxyMode is one of "rule" (default), "global" or "direct". Stored as a
	// string rather than an enum so an older build reading a newer file keeps
	// the value instead of dropping it.
	ProxyMode string `json:"proxy_mode,omitempty"`
	// ProxyGuard keeps the OS proxy registration pointed at us. Opt-in: while
	// it is on, switching the system proxy off from Windows is undone within
	// half a minute.
	ProxyGuard bool `json:"proxy_guard,omitempty"`
	// Language is the UI language ("zh" or "en"). The tray reads it too, so
	// the menu speaks the same language as the window instead of always
	// Chinese.
	Language     string `json:"language,omitempty"`
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
	// The option chips are part of the pushed state, not separate calls: the
	// window paints them from the snapshot it already has and keeps them
	// current from this event, so nothing has to be re-fetched on a tab
	// switch (which used to leave the chips dark until their own getter
	// answered).
	AutoStartCore bool `json:"auto_start_core"`
	LaunchAtLogin bool `json:"launch_at_login"`
	ProxyGuard    bool `json:"proxy_guard"`
	// TunRetrying says an automatic rebuild is scheduled after a failure:
	// the chip shows "auto-retrying" instead of demanding a manual click.
	TunRetrying bool `json:"tun_retrying"`
	// TunPending says the intent is on but the adapter is still coming up
	// (the boot bring-up takes several seconds): the chip shows the user's
	// switch as on instead of flashing off while the adapter builds.
	TunPending bool `json:"tun_pending"`
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
	sysProxyWanted bool // user intent, persisted in settings.json
	sysProxyActive bool // we registered the OS proxy and must undo it
	autoStartCore  bool // start the engine when the app launches
	launchAtLogin  bool // register the app to start with the OS login
	// launchAtLoginChecked marks the cache above as authoritative (the OS has
	// been asked at least once); probing keeps concurrent callers from each
	// spawning their own PowerShell query.
	launchAtLoginChecked bool
	launchAtLoginProbing bool
	tunWanted            bool   // TUN mode intent, persisted in settings.json
	tunActive            bool   // the virtual adapter is up and capturing traffic
	tunErr               string // last TUN failure, surfaced in the UI
	// tunRetries counts automatic rebuild attempts since the last success.
	// A failure schedules the next attempt with exponential backoff; only
	// exhausting the ladder hands the problem back to a manual click.
	tunRetries   int
	proxyMode    string // "rule" | "global" | "direct", persisted in settings.json
	proxyGuard   bool   // keep the OS proxy registration ours, persisted too
	language     string // UI language ("zh" | "en"), shared by window and tray
	appliedPorts string // ports registered with the OS, for stale cleanup

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
	// processResolver attributes relayed connections to OS processes for
	// the connections page; created once, refreshed per engine start with
	// the current listener ports.
	processResolver *processResolver
	// lifetimeDomains mirrors the engine's per-destination table for the
	// statistics page while the engine is stopped: seeded from the snapshot
	// at boot, refreshed from the live collector on every poll, served from
	// the cache when the engine is gone.
	lifetimeDomains []stats.DomainStat
	// lifetimeProcesses mirrors the per-application table the same way.
	lifetimeProcesses []stats.ProcessStat
	// lifetime metrics (peaks, max concurrency, on-time) cached the same way.
	lifetimePeakUp    int64
	lifetimePeakDown  int64
	lifetimeMaxConns  int64
	lifetimeActiveSec int64

	// probeKick wakes the self-check loop the moment the engine comes up, so
	// the first report lands without waiting a full minute. Buffered, so a
	// kick never blocks StartEngine; probing single-flights the probe set
	// itself, and lastProbe caches the newest report for the window and the
	// tray tooltip.
	probeKick chan struct{}
	probing   atomic.Bool
	lastProbe atomic.Pointer[ProbeReport]

	// Closed once Shutdown has torn TUN and the engine down. The TUN handover
	// waits on it before quitting, so the elevated successor never starts
	// building the adapter while this copy is still removing it.
	shutdownDone chan struct{}
	shutdownOnce sync.Once
}

// NewApp creates the App application struct that is bound to the frontend.
func NewApp() *App {
	// Rule-based routing is the default, and it has to be set here rather than
	// left empty: the tray and the window read this before Startup has loaded
	// the settings file, and an empty mode would render as "no mode selected".
	a := &App{
		proxyMode:       router.ModeRule.String(),
		shutdownDone:    make(chan struct{}),
		probeKick:       make(chan struct{}, 1),
		processResolver: newProcessResolver(nil, nil),
	}
	// The resolver needs the live TUN state to pick its lookup strategy; the
	// closure is safe to set after construction because it only fires once
	// connections start flowing.
	a.processResolver.tunActive = func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.tunActive
	}
	return a
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
		// Empty means "never chosen" (a settings file from before language
		// support, or a fresh install): the window decides then, and tells us.
		if st.Language != "" {
			a.language = normalizeLang(st.Language)
		}
	}
	a.mu.Unlock()
	// Keep the OS launch-at-login entry pointing at the current exe (the
	// binary may have been moved since it was registered). Off the startup
	// path: repairing the logon task costs a PowerShell launch, and nothing
	// waits for it. The probe right after it primes the cache the window
	// reads, so the "开机启动" chip is lit on the first paint.
	go func() {
		syncLaunchAtLogin(st)
		a.refreshLaunchAtLogin()
	}()
	a.loadLifetimeStats()
	go runTray(a)
	// A logon start is tray-only, so a double-click on the icon has to be able
	// to pull the window up from here.
	go WatchShowRequests(func() { showMainWindow(a) })
	// Drop restart.request into the data directory to swap in a fresh build.
	go a.restartWatcher()
	go a.proxyGuardLoop()
	// The automatic self-check: once per minute while the engine runs, and
	// once shortly after every engine start.
	go a.probeLoop()
	// Catch-up attribution for connections whose process row was not ready
	// (or was ambiguous) when they were accepted.
	go a.processAttributionLoop()
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
	// Everything is down: drop the single-instance claim so a waiting
	// successor (the elevated copy in a TUN handover) can take over, and tell
	// the handover it is safe to exit. Order matters -- the claim must be free
	// only once the adapter is gone.
	ReleaseSingleInstance()
	a.shutdownOnce.Do(func() { close(a.shutdownDone) })
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

	// The engine just came up: ask the self-check loop for an immediate
	// report once the listeners have settled — for every start, not only
	// the TUN path. Non-blocking: a kick still pending from a previous
	// start needs no queueing behind.
	select {
	case a.probeKick <- struct{}{}:
	default:
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

// SetSystemProxy toggles the system proxy. The intent is persisted and
// survives restarts; the WinINET registration itself takes effect
// immediately when the engine runs. While the TUN adapter is up the
// registration is withheld (TUN already captures every packet) — the intent
// stays saved and takes over the moment TUN goes down.
func (a *App) SetSystemProxy(on bool) error {
	a.mu.Lock()
	a.sysProxyWanted = on
	persistErr := a.persistSettingsLocked()
	tunUp, running := a.tunActive, a.running
	if !running || (on && tunUp) {
		// Nothing to register right now (engine down, or TUN owns the
		// traffic): the persisted intent is the change the user asked for.
		a.emitStatusLocked(nil)
		a.mu.Unlock()
		if persistErr != nil {
			loggo.Warn("[GUI] persist settings: %s", persistErr.Error())
		}
		return nil
	}
	err := a.applySystemProxyLocked(on)
	a.mu.Unlock()
	if persistErr != nil {
		loggo.Warn("[GUI] persist settings: %s", persistErr.Error())
	}
	return err
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

// GetLanguage returns the UI language the tray renders in, or "" when it has
// never been chosen (fresh install, or a settings file from before language
// support). The window owns the switcher: it reads this back on startup so the
// two surfaces cannot disagree, and answers "" with its own choice.
func (a *App) GetLanguage() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.language
}

// SetLanguage records the UI language and persists it. Nothing is emitted: the
// window is the one switching, and the tray re-reads the value on its next
// one-second refresh, which is how its menu follows along without the menu
// being rebuilt.
func (a *App) SetLanguage(lang string) error {
	a.mu.Lock()
	a.language = normalizeLang(lang)
	err := a.persistSettingsLocked()
	a.mu.Unlock()
	if err != nil {
		loggo.Warn("[GUI] persist settings: %s", err.Error())
	}
	return err
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
		// Repair before returning instead of waiting for the next tick: turning
		// the switch on means "make this true now". Synchronous on purpose —
		// a pending background repair would race with anything that changes
		// the registration right after this call.
		a.proxyGuardCheck()
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

// Automatic TUN rebuild: a failure — enable error or watchdog rollback —
// schedules applyTun again with exponential backoff. Bring-up races the
// physical network's own settling (routes, DNS, the resolver handoff), and
// tonight's logs show a transient blip costing a manual click when the
// watchdog was actually right to roll back. Only after the ladder runs out
// does the failure land in the chip as a manual retry.
const (
	tunRetryMax  = 4
	tunRetryBase = 5 * time.Second
	tunRetryCap  = 60 * time.Second
)

const (
	attributionRetryInterval = 2 * time.Second
	attributionMaxAge        = 30 * time.Second
)

// processAttributionLoop re-attempts process attribution for young sessions
// that were accepted before their connection-table row was ready or while
// the lookup was ambiguous. FindProcess caches misses for a couple of
// seconds, so the loop stays cheap even with a burst of new connections.
func (a *App) processAttributionLoop() {
	ticker := time.NewTicker(attributionRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.shutdownDone:
			return
		case <-ticker.C:
			a.mu.Lock()
			eng, res := a.engine, a.processResolver
			a.mu.Unlock()
			if eng == nil || res == nil {
				continue
			}
			for _, v := range eng.Stats().SessionsNeedingProcess(attributionMaxAge) {
				var srcIP net.IP
				var srcPort int
				if h, p, err := net.SplitHostPort(v.Client); err == nil {
					srcIP = net.ParseIP(h)
					srcPort, _ = strconv.Atoi(p)
				}
				if name, icon := res.FindProcess(srcIP, srcPort, v.Host, v.Port); name != "" {
					eng.Stats().SetSessionProcess(v.ID, name, icon)
				}
			}
		}
	}
}

// scheduleTunRetry arms one automatic applyTun re-run. The user's intent
// (tunWanted) is the switch: turning TUN off or quitting cancels the chain,
// and a non-elevated copy never auto-retries because its rebuild path is a
// full elevated handover, which needs the user's own consent (UAC).
func (a *App) scheduleTunRetry(reason string) {
	a.mu.Lock()
	if !a.tunWanted || a.quitting {
		a.mu.Unlock()
		return
	}
	a.tunRetries++
	if a.tunRetries > tunRetryMax {
		// The ladder is spent: clear the counter so the status flag stops
		// claiming an auto-retry is pending, and hand the problem to the
		// chip's manual retry.
		a.tunRetries = 0
		a.mu.Unlock()
		loggo.Error("[GUI] TUN auto-retry gave up after %d attempts (%s); waiting for a manual retry", tunRetryMax, reason)
		return
	}
	n := a.tunRetries
	wait := tunRetryBase << (n - 1)
	if wait > tunRetryCap {
		wait = tunRetryCap
	}
	a.mu.Unlock()
	loggo.Warn("[GUI] TUN failed (%s); automatic retry %d/%d in %v", reason, n, tunRetryMax, wait)
	go func() {
		select {
		case <-time.After(wait):
		case <-a.shutdownDone:
			return
		}
		_ = a.applyTun()
	}()
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
	// A pending auto-retry must not resurrect TUN after the user turned the
	// intent off while it waited.
	if !a.tunWanted {
		a.mu.Unlock()
		return errors.New("tun mode is not wanted")
	}
	running, cfg := a.running, a.lastCfg
	a.mu.Unlock()
	if !running {
		return uiErr("err.tunNeedRunning", nil)
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
			// The successor is waiting on the single-instance claim, but it
			// must not get it before this copy has torn TUN down: it would
			// bring the adapter up on top of a half-removed one and tun2socks
			// answers that with log.Fatalf. Shutdown releases the claim.
			a.armForcedExit("handover")
			// quitApp, not runtime.Quit: BeforeClose turns a bare Quit into a
			// hide-to-tray, so Shutdown would never run and the successor
			// would inherit a live adapter.
			a.quitApp()
			return nil
		}
		handoverErr := uiErr("err.tunAuthIncomplete", nil)
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
		a.scheduleTunRetry(err.Error())
		return err
	}
	// The adapter is up: flip the engine into TUN-mode DNS handling so every
	// captured :53 query is answered in-process (smart split + fake-ip) and
	// proxied domains reach the upstream by name instead of as real IPs.
	a.enableTUNDNS()
	a.mu.Lock()
	a.tunActive = true
	a.tunErr = ""
	// The TUN captures every packet system-wide, so the WinINET registration
	// would only double-apply: withdraw it while the adapter is up. The
	// user's intent (sysProxyWanted) must NOT be rewritten here — SetTunMode
	// re-applies the system proxy from that intent the moment TUN goes down,
	// and rewriting it turned "toggled on" into "silently off" across every
	// restart that brought TUN up.
	sysProxyRegistered := a.sysProxyActive
	a.mu.Unlock()
	if sysProxyRegistered {
		_ = a.applySystemProxyLocked(false)
	}
	a.mu.Lock()
	a.tunRetries = 0
	a.mu.Unlock()
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
	// recovers on its own. tunErr is a diagnostic flag, not display text (the
	// chip only checks that it is set), so it stays in English: anything that
	// reaches the screen comes from the catalogue.
	params.OnRollback = func(reason string) {
		a.disableTUNDNS()
		a.mu.Lock()
		a.tunActive = false
		a.tunErr = "watchdog rollback: " + reason
		a.mu.Unlock()
		a.emitStatus()
		a.scheduleTunRetry("watchdog rollback: " + reason)
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
	a.autoStartCore = on
	err := a.persistSettingsLocked()
	a.mu.Unlock()
	if err != nil {
		loggo.Warn("[GUI] persist settings: %s", err.Error())
	}
	a.emitStatus()
	return err
}

// GetLaunchAtLogin reports whether the app is registered to start with the
// OS login.
//
// The registry/task scheduler is the source of truth, but reading it on
// Windows means a PowerShell launch -- the better part of a second. The window
// asks for this every time it is opened or switched back to, so the answer is
// served from a cache that Startup fills right after launch and every toggle
// updates. Until that first probe lands, the persisted intent is the estimate,
// and the status event the probe emits corrects it.
func (a *App) GetLaunchAtLogin() bool {
	a.mu.Lock()
	if !a.launchAtLoginChecked && !a.launchAtLoginProbing {
		a.launchAtLoginProbing = true
		go a.refreshLaunchAtLogin()
	}
	on := a.launchAtLogin
	a.mu.Unlock()
	return on
}

// refreshLaunchAtLogin asks the OS where the registration actually stands and
// publishes the answer to the window.
func (a *App) refreshLaunchAtLogin() {
	on := launchAtLoginEnabled()
	a.mu.Lock()
	a.launchAtLogin = on
	a.launchAtLoginChecked = true
	a.launchAtLoginProbing = false
	a.mu.Unlock()
	a.emitStatus()
}

// SetLaunchAtLogin registers or removes the launch-at-login entry. On Windows
// it is a per-user Run-key entry that starts the app hidden in the tray;
// together with 自动连接 and 系统代理 this gives the full boot-time transparent
// proxy. The intent is persisted so a moved exe can be re-registered.
func (a *App) SetLaunchAtLogin(on bool) error {
	// Registered/removed first: if the OS refuses, the intent must not claim
	// success (the chip would then lie until the next probe).
	if on {
		if err := enableLaunchAtLogin(); err != nil {
			return err
		}
	} else if err := disableLaunchAtLogin(); err != nil {
		return err
	}
	a.mu.Lock()
	a.launchAtLogin = on
	// We just wrote the truth ourselves; no need to pay for a PowerShell
	// query to learn what we did.
	a.launchAtLoginChecked = true
	err := a.persistSettingsLocked()
	a.mu.Unlock()
	if err != nil {
		loggo.Warn("[GUI] persist settings: %s", err.Error())
	}
	a.emitStatus()
	return err
}

// persistSettingsLocked writes the current GUI settings to disk. Callers
// must hold a.mu.
func (a *App) persistSettingsLocked() error {
	return saveSettings(guiSettings{
		SystemProxy:   a.sysProxyWanted,
		AutoStartCore: a.autoStartCore,
		LaunchAtLogin: a.launchAtLogin,
		TunMode:       a.tunWanted,
		ProxyMode:     a.proxyMode,
		ProxyGuard:    a.proxyGuard,
		Language:      a.language,
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

// quitApp is the one real "exit now" path: stop the engine, then ask Wails to
// exit. Callers are the tray menu, the TUN handover and the restart watcher.
// The quitting flag must be set first, otherwise BeforeClose hides the window
// instead of letting the process go, and Shutdown never runs.
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
	ls := a.engine.Stats().LifetimeStats()
	a.lifetimeAll = o.Lifetime
	a.lifetimeDirect = o.LifetimeDirect
	a.lifetimeProxy = o.LifetimeProxy
	a.lifetimePeakUp = ls.PeakUp
	a.lifetimePeakDown = ls.PeakDown
	a.lifetimeMaxConns = ls.MaxConns
	a.lifetimeActiveSec = ls.ActiveSec
	return &o
}

// loadLifetimeStats seeds the lifetime cache from the persisted stats file so
// the permanent record is visible before the engine starts.
func (a *App) loadLifetimeStats() {
	all, direct, proxy := stats.LifetimeTotals(statsPath())
	a.mu.Lock()
	a.lifetimeAll, a.lifetimeDirect, a.lifetimeProxy = all, direct, proxy
	a.lifetimeDomains = stats.DomainTable(statsPath())
	a.lifetimeProcesses = stats.ProcessTable(statsPath())
	a.lifetimePeakUp, a.lifetimePeakDown, a.lifetimeMaxConns, a.lifetimeActiveSec = stats.LifetimeMetrics(statsPath())
	a.mu.Unlock()
}

// GetLifetimeStats returns the permanent traffic record with its domestic
// (direct) and foreign (proxy) shares, plus the lifetime monitoring
// metrics. The counters survive restarts and are never cleared except by
// ResetLifetimeStats.
func (a *App) GetLifetimeStats() stats.LifetimeStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return stats.LifetimeStats{
		Totals:    a.lifetimeAll,
		Direct:    a.lifetimeDirect,
		Proxy:     a.lifetimeProxy,
		PeakUp:    a.lifetimePeakUp,
		PeakDown:  a.lifetimePeakDown,
		MaxConns:  a.lifetimeMaxConns,
		ActiveSec: a.lifetimeActiveSec,
	}
}

// GetLifetimeDomains returns the permanent per-destination record: one entry
// per domain (or bare address) with its domestic/foreign byte split, going
// live from the collector while the engine runs and from the cached snapshot
// copy while it is stopped. The table survives restarts; it is cleared only
// by ResetLifetimeStats.
func (a *App) GetLifetimeDomains(limit int) []stats.DomainStat {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running && a.engine != nil {
		out := a.engine.Stats().Domains(limit, "bytes")
		a.lifetimeDomains = out
		return out
	}
	out := a.lifetimeDomains
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ResetLifetimeStats clears the permanent traffic record. The per-destination
// table is cleared with it: the statistics page shows both as one permanent
// record, and leaving domains behind after a reset would read as lost bytes.
// While the engine is stopped the stats file is rewritten directly, so the
// cleared totals are not resurrected by the next launch.
func (a *App) ResetLifetimeStats() error {
	a.mu.Lock()
	if a.running && a.engine != nil {
		st := a.engine.Stats()
		st.LifetimeReset()
		st.ResetDomains()
		st.ResetProcesses()
		a.lifetimeAll, a.lifetimeDirect, a.lifetimeProxy = stats.Counters{}, stats.Counters{}, stats.Counters{}
		a.lifetimeDomains = nil
		a.lifetimeProcesses = nil
		a.lifetimePeakUp, a.lifetimePeakDown, a.lifetimeMaxConns, a.lifetimeActiveSec = 0, 0, 0, 0
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()

	c := stats.New(stats.Config{File: statsPath(), SaveInterval: 0})
	c.LifetimeReset()
	c.ResetDomains()
	c.ResetProcesses()
	err := c.Save()
	c.Close()
	a.mu.Lock()
	a.lifetimeAll, a.lifetimeDirect, a.lifetimeProxy = stats.Counters{}, stats.Counters{}, stats.Counters{}
	a.lifetimeDomains = nil
	a.lifetimeProcesses = nil
	a.lifetimePeakUp, a.lifetimePeakDown, a.lifetimeMaxConns, a.lifetimeActiveSec = 0, 0, 0, 0
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
	// Never sample finer than the stored history: one bucket of traffic landing
	// in one slot of a step that is half its width reads as a spike followed by
	// a zero, and the plotted rate is doubled. Round up to a whole number of
	// buckets so every step owns exact, complete buckets.
	bucket := a.engine.Stats().BucketInterval()
	if bucket <= 0 {
		bucket = time.Minute
	}
	if step < bucket {
		step = bucket
	}
	if rem := step % bucket; rem != 0 {
		step += bucket - rem
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
	// The list is capped so a burst of short-lived connections cannot make the
	// one-second poll expensive; the overview still reports the true total, and
	// the connections page shows it, so a capped list is visible rather than
	// silently short.
	return a.engine.Stats().ActiveSessions(stats.ActiveOptions{Limit: 1000})
}

// GetNodeStatuses returns the live per-node availability from the manager's
// real-request probes: SOCKS5 handshake plus an HTTP GET through the node
// itself. Nil while the engine is stopped — without a manager there is
// nothing to probe.
func (a *App) GetNodeStatuses() []sppclient.NodeStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running || a.engine == nil {
		return nil
	}
	return a.engine.NodeStatuses()
}

// GetLifetimeProcesses returns the permanent per-application record behind
// the statistics page's application view, split by routing decision like
// the domain table. Served live while the engine runs, from the cached
// snapshot copy while it is stopped; cleared by ResetLifetimeStats.
func (a *App) GetLifetimeProcesses() []stats.ProcessStat {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running && a.engine != nil {
		out := a.engine.Stats().Processes(500)
		a.lifetimeProcesses = out
		return out
	}
	out := a.lifetimeProcesses
	if len(out) > 500 {
		out = out[:500]
	}
	return out
}

// GetProcesses returns the live per-application table behind the connections
// page's application tab: cumulative bytes plus the speeds sampled since the
// previous call. The tab polls once a second, so the sampling window matches
// what the table shows; nil while the engine is stopped.
func (a *App) GetProcesses() []stats.ProcessStat {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running || a.engine == nil {
		return nil
	}
	return a.engine.Stats().Processes(500)
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
	merged := fileCfg.MergeWithEngineConfig(base)
	// The resolver needs the listener ports to exclude them from local-port
	// lookups; they can change between engine restarts.
	a.processResolver.UpdateListenPorts([]string{merged.Socks5Listen, merged.HTTPListen})
	merged.ProcessResolver = a.processResolver
	return merged, nil
}

func (a *App) statusLocked() EngineStatus {
	st := EngineStatus{
		Running:       a.running,
		SystemProxy:   a.sysProxyActive || a.sysProxyWanted,
		Tun:           a.tunActive,
		TunError:      a.tunErr,
		ProxyMode:     a.proxyMode,
		Error:         "",
		AutoStartCore: a.autoStartCore,
		LaunchAtLogin: a.launchAtLogin,
		ProxyGuard:    a.proxyGuard,
		TunRetrying:   a.tunWanted && !a.tunActive && a.tunRetries > 0,
		TunPending:    a.tunWanted && !a.tunActive && a.tunErr == "",
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
