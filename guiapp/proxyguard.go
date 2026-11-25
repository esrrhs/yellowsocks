package guiapp

import (
	"time"

	"github.com/esrrhs/gohome/loggo"
)

// System proxy guard.
//
// The OS proxy registration is a single per-user registry value, and any other
// proxy client, VPN or "network optimiser" is free to overwrite it. Without a
// guard the user's traffic silently leaves through whatever was registered
// last, which is exactly the failure that is hardest to notice: everything
// still works, just not through this app.
//
// The guard is opt-in (settings.json: proxy_guard). Turning it on makes the
// switch authoritative, which also means the user cannot turn the system proxy
// off from the Windows settings page while it is enabled — that surprise is why
// the default is off.

// proxyGuardInterval is deliberately slow: this is a repair loop, not a race.
const proxyGuardInterval = 30 * time.Second

// proxyGuardLoop runs for the lifetime of the process and does nothing unless
// the guard is on and the registration is currently ours.
func (a *App) proxyGuardLoop() {
	ticker := time.NewTicker(proxyGuardInterval)
	defer ticker.Stop()
	for range ticker.C {
		a.proxyGuardCheck()
	}
}

func (a *App) proxyGuardCheck() {
	a.mu.Lock()
	active := a.proxyGuard && a.sysProxyActive && a.running
	cfg := a.lastCfg
	a.mu.Unlock()
	if !active {
		return
	}
	httpPort, socksPort, err := proxyPorts(cfg.HTTPListen, cfg.Socks5Listen)
	if err != nil {
		return
	}
	// Still pointing at us: nothing to repair.
	if sysproxyEnabled() && sysproxyProxyServerContains(httpPort, socksPort) {
		return
	}
	loggo.Warn("[GUI] system proxy was changed by another program; restoring it")
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.applySystemProxyLocked(true); err != nil {
		loggo.Warn("[GUI] proxy guard could not restore the system proxy: %s", err.Error())
	}
}
