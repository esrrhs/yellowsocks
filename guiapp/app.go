package guiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/yellowsocks/core"
	"github.com/esrrhs/yellowsocks/core/config"
	"github.com/esrrhs/yellowsocks/core/stats"
	"github.com/esrrhs/yellowsocks/core/version"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const configName = "config.yaml"
const settingsName = "settings.json"

// guiSettings is the GUI-only state that does not belong in the CLI config
// file, persisted next to it.
type guiSettings struct {
	SystemProxy   bool   `json:"system_proxy"`
	AutoStartCore bool   `json:"auto_start_core"`
	AppliedPorts  string `json:"sysproxy_applied_ports,omitempty"`
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
	Socks5      string `json:"socks5"`
	HTTP        string `json:"http"`
	DNS         string `json:"dns"`
	SPPServer   string `json:"spp_server"`
	SPPProto    string `json:"spp_proto"`
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
	appliedPorts   string // ports registered with the OS, for stale cleanup
}

// NewApp creates the App application struct that is bound to the frontend.
func NewApp() *App {
	return &App{}
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
	}
	a.mu.Unlock()
	go runTray(a)
	if st.AutoStartCore {
		go func() { _ = a.StartEngine() }()
	}
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

// shutdown is called on real application exit. It stops the engine, removes
// the tray icon and undoes the system proxy registration.
func (a *App) Shutdown(ctx context.Context) {
	a.mu.Lock()
	a.quitting = true
	running := a.running
	active := a.sysProxyActive
	a.mu.Unlock()
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
	return nil
}

// StopEngine stops the proxy engine. Safe to call when it is not running.
func (a *App) StopEngine() error {
	a.mu.Lock()
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

// persistSettingsLocked writes the current GUI settings to disk. Callers
// must hold a.mu.
func (a *App) persistSettingsLocked() error {
	return saveSettings(guiSettings{
		SystemProxy:   a.sysProxyWanted,
		AutoStartCore: a.autoStartCore,
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
// is stopped.
func (a *App) GetOverview() *stats.Overview {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running || a.engine == nil {
		return nil
	}
	o := a.engine.Stats().Overview()
	return &o
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

// configPath is the per-user config file location, shared in style with the
// CLI's EnsureWritableWorkDir fallback (e.g. %APPDATA%/YellowSocks).
func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "YellowSocks", configName), nil
}

func writeDefaultConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(defaultConfig), 0644)
}

// settingsPath lives next to config.yaml so users can find both together.
func settingsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "YellowSocks", settingsName), nil
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
	base := core.EngineConfig{
		DNSListen: "127.0.0.1:1053",
	}
	return fileCfg.MergeWithEngineConfig(base), nil
}

func (a *App) statusLocked() EngineStatus {
	st := EngineStatus{Running: a.running, SystemProxy: a.sysProxyActive || a.sysProxyWanted, Error: ""}
	if a.running {
		st.Socks5 = a.lastCfg.Socks5Listen
		st.HTTP = a.lastCfg.HTTPListen
		st.DNS = a.lastCfg.DNSListen
		st.SPPServer = a.lastCfg.SPPServer
		st.SPPProto = a.lastCfg.SPPProto
	}
	return st
}

// emitStatusLocked pushes the current state to the frontend. Callers must
// hold a.mu since it reads engine state.
func (a *App) emitStatusLocked(err error) {
	st := a.statusLocked()
	if err != nil {
		st.Error = err.Error()
	}
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "engine:status", st)
	}
}
