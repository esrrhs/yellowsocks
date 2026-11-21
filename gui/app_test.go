package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useTempConfigDir redirects os.UserConfigDir (AppData on Windows, HOME on
// darwin) into a fresh temp directory so tests never touch the real
// %APPDATA%/YellowSocks config.
func useTempConfigDir(t *testing.T) {
	t.Helper()
	if isWindows() {
		t.Setenv("AppData", t.TempDir())
	} else {
		t.Setenv("HOME", t.TempDir())
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
}
