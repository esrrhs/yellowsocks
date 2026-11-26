//go:build windows

package guiapp

import (
	"testing"

	"golang.org/x/sys/windows/registry"
)

type inetSnapshot struct {
	enable     uint32
	enableOK   bool
	server     string
	serverOK   bool
	override   string
	overrideOK bool
}

func snapshotInternetSettings(t *testing.T) inetSnapshot {
	t.Helper()
	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("open internet settings: %v", err)
	}
	defer key.Close()
	var snap inetSnapshot
	if v, _, err := key.GetIntegerValue("ProxyEnable"); err == nil {
		snap.enable, snap.enableOK = uint32(v), true
	}
	if v, _, err := key.GetStringValue("ProxyServer"); err == nil {
		snap.server, snap.serverOK = v, true
	}
	if v, _, err := key.GetStringValue("ProxyOverride"); err == nil {
		snap.override, snap.overrideOK = v, true
	}
	return snap
}

func restoreInternetSettings(t *testing.T, snap inetSnapshot) {
	t.Helper()
	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		t.Errorf("restore: open internet settings: %v", err)
		return
	}
	defer key.Close()
	if snap.enableOK {
		_ = key.SetDWordValue("ProxyEnable", snap.enable)
	} else {
		_ = key.SetDWordValue("ProxyEnable", 0)
	}
	restoreString(key, "ProxyServer", snap.server, snap.serverOK)
	restoreString(key, "ProxyOverride", snap.override, snap.overrideOK)
	refreshWinINET()
}

func restoreString(key registry.Key, name, value string, ok bool) {
	if ok {
		_ = key.SetStringValue(name, value)
		return
	}
	_ = key.DeleteValue(name)
}

func useInternetSettingsRestore(t *testing.T) {
	t.Helper()
	snap := snapshotInternetSettings(t)
	t.Cleanup(func() { restoreInternetSettings(t, snap) })
}

func TestSysproxyEnableDisableRoundTrip(t *testing.T) {
	useInternetSettingsRestore(t)

	const httpPort, socksPort = 18080, 19080
	if err := sysproxyEnable(httpPort, socksPort); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !sysproxyEnabled() {
		t.Fatal("ProxyEnable was not set")
	}
	if !sysproxyProxyServerContains(httpPort, socksPort) {
		t.Fatal("ProxyServer does not mention the registered ports")
	}

	if err := sysproxyDisable(); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if sysproxyEnabled() {
		t.Fatal("ProxyEnable still on after disable")
	}
}

func TestSetSystemProxyAppliesWhileEngineRuns(t *testing.T) {
	useTempConfigDir(t)
	writeDefaultEngineConfig(t)
	useInternetSettingsRestore(t)

	a := NewApp()
	if err := a.StartEngine(); err != nil {
		t.Skipf("cannot start engine in this environment: %v", err)
	}
	t.Cleanup(func() { _ = a.StopEngine() })

	if err := a.SetSystemProxy(true); err != nil {
		t.Fatalf("SetSystemProxy(true): %v", err)
	}
	if !a.GetSystemProxy() {
		t.Fatal("intent was not persisted as on")
	}
	if !sysproxyEnabled() || !sysproxyProxyServerContains(8080, 10808) {
		t.Fatal("system proxy was not registered for the engine ports")
	}

	if err := a.SetSystemProxy(false); err != nil {
		t.Fatalf("SetSystemProxy(false): %v", err)
	}
	if a.GetSystemProxy() {
		t.Fatal("intent was not persisted as off")
	}
	if sysproxyEnabled() {
		t.Fatal("system proxy still enabled after turning it off")
	}
}

func TestProxyGuardRestoresOverwrittenRegistration(t *testing.T) {
	useTempConfigDir(t)
	writeDefaultEngineConfig(t)
	useInternetSettingsRestore(t)

	a := NewApp()
	if err := a.StartEngine(); err != nil {
		t.Skipf("cannot start engine in this environment: %v", err)
	}
	t.Cleanup(func() { _ = a.StopEngine() })

	if err := a.SetSystemProxy(true); err != nil {
		t.Fatalf("SetSystemProxy: %v", err)
	}
	if err := a.SetProxyGuard(true); err != nil {
		t.Fatalf("SetProxyGuard: %v", err)
	}

	// Simulate another program turning the system proxy off.
	if err := sysproxyDisable(); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if sysproxyEnabled() {
		t.Fatal("precondition: overwrite did not disable the proxy")
	}

	a.proxyGuardCheck()
	if !sysproxyEnabled() || !sysproxyProxyServerContains(8080, 10808) {
		t.Fatal("proxy guard did not restore the registration")
	}
}

func TestCleanupStaleSystemProxyRemovesOurPorts(t *testing.T) {
	useTempConfigDir(t)
	useInternetSettingsRestore(t)

	const httpPort, socksPort = 18080, 19080
	if err := sysproxyEnable(httpPort, socksPort); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := saveSettings(guiSettings{AppliedPorts: "18080;19080"}); err != nil {
		t.Fatalf("saveSettings: %v", err)
	}

	CleanupStaleSystemProxy()
	if sysproxyEnabled() {
		t.Fatal("stale system proxy was left enabled")
	}
}
