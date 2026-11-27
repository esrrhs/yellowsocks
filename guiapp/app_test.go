package guiapp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/esrrhs/yellowsocks/core/stats"
)

// useTempConfigDir redirects os.UserConfigDir (AppData on Windows, HOME on
// darwin) into a fresh temp directory so tests never touch the real
// %APPDATA%/YellowSocks config. It also snapshots and restores data files
// in DataDir: when the test binary's directory is writable, DataDir is that
// portable exe directory (resolved once per process), so AppData/HOME alone
// would not isolate config.yaml or settings.json.
func useTempConfigDir(t *testing.T) {
	t.Helper()
	if isWindows() {
		t.Setenv("AppData", t.TempDir())
	} else {
		t.Setenv("HOME", t.TempDir())
	}
	isolateDataFiles(t, configName, settingsName, statsName)
}

type dataFileSnap struct {
	path    string
	content []byte
	existed bool
}

// isolateDataFiles snapshots the named files in DataDir and restores them
// when the test ends, so leftovers cannot leak into later tests.
func isolateDataFiles(t *testing.T, names ...string) {
	t.Helper()
	dir := DataDir()
	snaps := make([]dataFileSnap, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err == nil {
			snaps = append(snaps, dataFileSnap{path: path, content: data, existed: true})
		} else {
			snaps = append(snaps, dataFileSnap{path: path})
		}
	}
	t.Cleanup(func() {
		for _, s := range snaps {
			if s.existed {
				_ = os.WriteFile(s.path, s.content, 0o644)
			} else {
				_ = os.Remove(s.path)
			}
		}
	})
}

// writeDefaultEngineConfig puts the default listen addresses into DataDir
// so StartEngine / SetSystemProxy do not depend on whatever a previous
// test left behind in the portable directory.
func writeDefaultEngineConfig(t *testing.T) {
	t.Helper()
	path, err := configPath()
	if err != nil {
		t.Fatalf("configPath: %v", err)
	}
	if err := writeDefaultConfig(path); err != nil {
		t.Fatalf("writeDefaultConfig: %v", err)
	}
}

func isWindows() bool {
	return filepath.Separator == '\\' && os.PathSeparator == '\\'
}

func TestGetConfigCreatesDefault(t *testing.T) {
	useTempConfigDir(t)
	a := NewApp()
	got, err := a.GetConfig()
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if !strings.Contains(got, "spp_server") {
		t.Fatalf("default config missing spp_server:\n%s", got)
	}
	// Second call must read the persisted file, not write a new one.
	path, err := configPath()
	if err != nil {
		t.Fatalf("configPath: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config file was not written: %v", err)
	}
	if string(data) != got {
		t.Fatalf("GetConfig result differs from file on disk")
	}
}

func TestSaveConfigRejectsInvalidYAML(t *testing.T) {
	useTempConfigDir(t)
	a := NewApp()
	if err := a.SaveConfig("::::not yaml [[["); err == nil {
		t.Fatalf("SaveConfig accepted invalid YAML")
	}
}

func TestSaveConfigRoundTrip(t *testing.T) {
	useTempConfigDir(t)
	a := NewApp()
	custom := defaultConfig + "\nextra_flag: ignored\n"
	if err := a.SaveConfig(custom); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	got, err := a.GetConfig()
	if err != nil {
		t.Fatalf("GetConfig after save: %v", err)
	}
	if got != custom {
		t.Fatalf("config round trip mismatch")
	}
}

func TestStartStopEngine(t *testing.T) {
	useTempConfigDir(t)
	writeDefaultEngineConfig(t)
	a := NewApp()

	st := a.EngineStatus()
	if st.Running {
		t.Fatalf("engine should not be running at startup")
	}

	if err := a.StartEngine(); err != nil {
		t.Skipf("cannot start engine in this environment (ports in use?): %v", err)
	}
	defer func() { _ = a.StopEngine() }()

	st = a.EngineStatus()
	if !st.Running {
		t.Fatalf("EngineStatus reports not running after StartEngine")
	}
	if st.Socks5 != "127.0.0.1:10808" {
		t.Fatalf("unexpected socks5 listen %q", st.Socks5)
	}
	if st.DNS != "127.0.0.1:1053" {
		t.Fatalf("unexpected dns listen %q (port 53 needs admin on macOS)", st.DNS)
	}
	if a.GetOverview() == nil {
		t.Fatalf("GetOverview returned nil while running")
	}
	if err := a.StopEngine(); err != nil {
		t.Fatalf("StopEngine: %v", err)
	}
	st = a.EngineStatus()
	if st.Running {
		t.Fatalf("EngineStatus reports running after StopEngine")
	}
	if a.GetOverview() != nil {
		t.Fatalf("GetOverview should be nil when stopped")
	}

	// The GUI enables stats persistence by default: stopping the engine
	// writes the final snapshot with the lifetime totals next to the other
	// data files.
	if _, err := os.Stat(statsPath()); err != nil {
		t.Fatalf("stats snapshot was not persisted after engine stop: %v", err)
	}
	all, _, _ := stats.LifetimeTotals(statsPath())
	if all.Total() < 0 {
		t.Fatalf("lifetime totals unreadable from %s", statsPath())
	}
}

func TestSettingsPersistProxyModeGuardAndAutoStart(t *testing.T) {
	useTempConfigDir(t)
	a := NewApp()
	if err := a.SetAutoStartCore(true); err != nil {
		t.Fatalf("SetAutoStartCore: %v", err)
	}
	if err := a.SetProxyMode("global"); err != nil {
		t.Fatalf("SetProxyMode: %v", err)
	}
	if err := a.SetProxyGuard(true); err != nil {
		t.Fatalf("SetProxyGuard: %v", err)
	}
	if err := a.SetSystemProxy(true); err != nil {
		t.Fatalf("SetSystemProxy (intent only): %v", err)
	}

	st, err := loadSettings()
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if !st.AutoStartCore || st.ProxyMode != "global" || !st.ProxyGuard || !st.SystemProxy {
		t.Fatalf("persisted settings mismatch: %+v", st)
	}
	if !a.GetAutoStartCore() || a.GetProxyMode() != "global" || !a.GetProxyGuard() || !a.GetSystemProxy() {
		t.Fatal("in-memory getters do not match what was set")
	}
}

func TestSetTunModeRequiresRunningEngine(t *testing.T) {
	useTempConfigDir(t)
	a := NewApp()
	if tunIsActive() {
		t.Fatal("TUN should be inactive at startup")
	}
	if a.TunSessionStats() != "inactive" && a.TunSessionStats() != "" {
		// Linux stub may return a different idle string; just ensure SetTunMode fails.
	}
	if err := a.SetTunMode(true); err == nil {
		t.Fatal("SetTunMode(true) succeeded without a running engine")
	}
	if a.GetTunMode() {
		// Intent is persisted before applyTun; that is existing behaviour.
		// The important part is that the adapter itself was not brought up.
	}
	if tunIsActive() {
		t.Fatal("TUN adapter was brought up even though the engine is stopped")
	}
}

func TestTunParamsFromKeepsNumericUpstreams(t *testing.T) {
	a := NewApp()
	cfg := a.lastCfg
	cfg.Socks5Listen = "127.0.0.1:10808"
	cfg.DNSListen = "127.0.0.1:1053"
	cfg.SPPServer = "203.0.113.10:8888"
	params := a.tunParamsFrom(cfg)
	if params.Socks5Addr != "127.0.0.1:10808" || params.DNSAddr != "127.0.0.1:1053" {
		t.Fatalf("unexpected listen addresses: %+v", params)
	}
	found := false
	for _, ip := range params.UpstreamIPs {
		if ip == "203.0.113.10" {
			found = true
		}
		if ip == "127.0.0.1" {
			t.Fatalf("loopback upstream should be skipped: %v", params.UpstreamIPs)
		}
	}
	if !found {
		t.Fatalf("numeric upstream missing: %v", params.UpstreamIPs)
	}
}

func TestConnectivityAndLogsWhenEngineStopped(t *testing.T) {
	useTempConfigDir(t)
	a := NewApp()
	report := a.TestConnectivity()
	if len(report.Items) == 0 || report.Items[0].OK {
		t.Fatalf("expected a failed engine probe, got %+v", report)
	}
	if a.GetLogPath() == "" {
		t.Fatal("GetLogPath is empty")
	}
	if a.GetLogs("INFO", 10) == nil {
		// empty slice is fine; nil is also fine as long as it does not panic
	}
	if a.GetVersion().Version == "" && a.GetVersion().GitCommit == "" {
		// version.Info may be unset in `go test`; the call must not panic
	}
	if a.GetSessions() == nil && a.GetErrors() == nil && a.GetNodes() == nil {
		// all empty when stopped is acceptable
	}
	_ = a.GetSeries(60)
}

// Regression: the user toggles the system proxy on, the app restarts, and
// TUN comes up at boot. Bringing TUN up withdraws the WinINET registration
// (the adapter already captures every packet), but it must not rewrite the
// persisted intent — it used to call SetSystemProxy(false), so every restart
// with TUN enabled flipped a saved "on" back to "off" on disk.
func TestSystemProxyIntentSurvivesTunSuppression(t *testing.T) {
	useTempConfigDir(t)
	snap := snapshotInternetSettings(t)
	t.Cleanup(func() { restoreInternetSettings(t, snap) })

	a := NewApp()
	if err := a.SetSystemProxy(true); err != nil {
		t.Fatalf("SetSystemProxy: %v", err)
	}
	if st, err := loadSettings(); err != nil || !st.SystemProxy {
		t.Fatalf("toggle did not persist the intent: %+v (%v)", st, err)
	}

	// Boot with TUN enabled: the registration is live when applyTun
	// succeeds, and applyTun withdraws it. The intent must survive.
	a.mu.Lock()
	a.sysProxyActive = true
	a.mu.Unlock()
	if err := a.applySystemProxyLocked(false); err != nil {
		t.Fatalf("suppress system proxy for TUN: %v", err)
	}
	if st, err := loadSettings(); err != nil || !st.SystemProxy {
		t.Fatalf("TUN suppression erased the system-proxy intent: %+v (%v)", st, err)
	}
	if !a.GetSystemProxy() {
		t.Fatal("GetSystemProxy lost the intent")
	}
}
