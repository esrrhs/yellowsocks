package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	Running   bool   `json:"running"`
	Socks5    string `json:"socks5"`
	HTTP      string `json:"http"`
	DNS       string `json:"dns"`
	SPPServer string `json:"spp_server"`
	SPPProto  string `json:"spp_proto"`
	Error     string `json:"error"`
}

// App is the structure bound to the Wails frontend. Every exported method
// becomes callable from JavaScript (see frontend/wailsjs).
type App struct {
	ctx context.Context

	mu       sync.Mutex
	engine   *core.Engine
	lastCfg  core.EngineConfig
	running  bool
	quitting bool
}

// NewApp creates the App application struct that is bound to the frontend.
func NewApp() *App {
	return &App{}
}

// startup is called when the app starts. The context is saved so we can call
// the Wails runtime, then the tray is started in its own goroutine.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	go runTray(a)
}

// beforeClose intercepts the window close event. Instead of quitting, the
// window hides to the tray; real quits go through the tray menu, which sets
// quitting first.
func (a *App) beforeClose(ctx context.Context) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.quitting {
		return false
	}
	runtime.WindowHide(ctx)
	return true
}

// shutdown is called on real application exit. It stops the engine and
// removes the tray icon.
func (a *App) shutdown(ctx context.Context) {
	a.mu.Lock()
	a.quitting = true
	running := a.running
	a.mu.Unlock()
	if running {
		_ = a.StopEngine()
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
	return nil
}

// StopEngine stops the proxy engine. Safe to call when it is not running.
func (a *App) StopEngine() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running {
		return nil
	}
	err := a.engine.Stop()
	a.engine = nil
	a.running = false
	a.emitStatusLocked(err)
	loggo.Info("[GUI] engine stopped")
	return err
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
	st := EngineStatus{Running: a.running, Error: ""}
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
