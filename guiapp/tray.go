package guiapp

import (
	_ "embed"
	"encoding/binary"
	"fmt"
	goruntime "runtime"
	"time"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/energye/systray"
)

//go:embed appicon.png
var appIconPNG []byte

// runTray populates and serves the system tray menu. It runs in its own
// goroutine started by App.startup. energye/systray is the community fork
// designed to coexist with Wails' event loop.
//
// The menu mirrors the dashboard instead of offering a second, independent set
// of switches: every title is refreshed from the same state the window reads,
// once a second. A tray that silently disagrees with the window is worse than
// no tray at all.
func runTray(a *App) {
	stop := make(chan struct{})
	systray.Run(func() {
		systray.SetIcon(trayIcon())
		systray.SetTooltip("YellowSocks")

		// Every label comes from the shared catalogue, in the language the
		// window is using. The titles set here are the first paint; refreshTray
		// rewrites them once a second, which is also how a language switch in
		// the window reaches the menu without the menu being rebuilt.
		lang := a.GetLanguage()
		m := &trayMenu{}

		// Status lines: disabled items used as read-only text. Two of them
		// keep the icon useful without opening the window.
		m.status = systray.AddMenuItem(tr(lang, "tray.stopped", nil), "Current transfer rate")
		m.status.Disable()
		m.conns = systray.AddMenuItem(tr(lang, "tray.conns", map[string]string{"n": "0"}), "Active connections")
		m.conns.Disable()
		systray.AddSeparator()

		m.show = systray.AddMenuItem(tr(lang, "tray.show", nil), "Show the YellowSocks window")
		systray.AddSeparator()
		m.start = systray.AddMenuItem(tr(lang, "tray.start", nil), "Start the proxy engine")
		m.stop = systray.AddMenuItem(tr(lang, "tray.stop", nil), "Stop the proxy engine")
		m.stop.Disable()
		m.sysProxy = systray.AddMenuItem(tr(lang, "tray.systemProxyOff", nil), "Toggle the OS-wide system proxy")
		m.tun = systray.AddMenuItem(tr(lang, "tray.tunOff", nil), "Toggle TUN mode (needs administrator)")

		m.mode = systray.AddMenuItem(tr(lang, "tray.mode", nil), "Routing mode")
		m.rule = m.mode.AddSubMenuItemCheckbox(tr(lang, "tray.modeRule", nil), "Domestic direct, foreign proxied", true)
		m.global = m.mode.AddSubMenuItemCheckbox(tr(lang, "tray.modeGlobal", nil), "Everything through the proxy", false)
		m.direct = m.mode.AddSubMenuItemCheckbox(tr(lang, "tray.modeDirect", nil), "Everything direct", false)

		systray.AddSeparator()
		m.quit = systray.AddMenuItem(tr(lang, "tray.quit", nil), "Quit YellowSocks")

		// A left click on the icon is the reflex for "open it again" once the
		// window has been closed to the tray, and on Windows it did nothing
		// at all before: only the menu item was wired. The menu keeps its own
		// entry as the discoverable equivalent, and the right click still
		// opens the menu because only the left button is claimed here.
		systray.SetOnClick(func(systray.IMenu) { showMainWindow(a) })
		m.show.Click(func() { showMainWindow(a) })
		m.start.Click(func() {
			go func() {
				_ = a.StartEngine()
				refreshTray(a, m)
			}()
		})
		m.stop.Click(func() {
			go func() {
				_ = a.StopEngine()
				refreshTray(a, m)
			}()
		})
		m.sysProxy.Click(func() {
			go func() {
				_ = a.SetSystemProxy(!a.GetSystemProxy())
				refreshTray(a, m)
			}()
		})
		m.tun.Click(func() {
			// SetTunMode can block for the UAC handover; never do that on the
			// menu's own goroutine, which also serves the window messages.
			go func() {
				_ = a.SetTunMode(!a.GetTunMode())
				refreshTray(a, m)
			}()
		})
		m.rule.Click(func() { setModeFromTray(a, "rule", m) })
		m.global.Click(func() { setModeFromTray(a, "global", m) })
		m.direct.Click(func() { setModeFromTray(a, "direct", m) })
		m.quit.Click(a.quitApp)

		refreshTray(a, m)

		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					refreshTray(a, m)
				case <-stop:
					return
				}
			}
		}()
	}, func() { close(stop) })
}

// trayMenu groups the items refreshTray rewrites. One value instead of a
// ten-argument list: the refresh runs from the ticker and from every click
// handler, and a new item is easy to forget in half of those call sites.
type trayMenu struct {
	status   *systray.MenuItem
	conns    *systray.MenuItem
	show     *systray.MenuItem
	start    *systray.MenuItem
	stop     *systray.MenuItem
	sysProxy *systray.MenuItem
	tun      *systray.MenuItem
	mode     *systray.MenuItem
	rule     *systray.MenuItem
	global   *systray.MenuItem
	direct   *systray.MenuItem
	quit     *systray.MenuItem
}

// setModeFromTray applies a mode and moves the checkmark, reading the mode back
// afterwards instead of trusting the click: the backend normalises unknown
// values and is the only thing that knows what actually took effect.
func setModeFromTray(a *App, mode string, m *trayMenu) {
	go func() {
		if err := a.SetProxyMode(mode); err != nil {
			return
		}
		applyModeChecks(a.GetProxyMode(), m)
	}()
}

func applyModeChecks(mode string, m *trayMenu) {
	if mode == "global" {
		m.global.Check()
	} else {
		m.global.Uncheck()
	}
	if mode == "direct" {
		m.direct.Check()
	} else {
		m.direct.Uncheck()
	}
	if mode == "rule" {
		m.rule.Check()
	} else {
		m.rule.Uncheck()
	}
}

// refreshTray pulls one snapshot and rewrites the menu titles. Everything is
// read through the App methods, so the tray cannot drift from the window --
// language included: the titles are re-rendered from the catalogue on every
// tick, so switching the language in the window changes the menu within a
// second without restarting anything.
func refreshTray(a *App, m *trayMenu) {
	defer func() {
		// The menu is destroyed at exit while this may still be in flight;
		// a panic here would take down the shutdown path.
		_ = recover()
	}()

	st := a.EngineStatus()
	lang := a.GetLanguage()
	text := func(key string, vars map[string]string) string { return tr(lang, key, vars) }

	if st.Running {
		m.start.Disable()
		m.stop.Enable()
	} else {
		m.start.Enable()
		m.stop.Disable()
	}

	active := 0
	up, down := int64(0), int64(0)
	if ov := a.GetOverview(); ov != nil {
		active = ov.ActiveConns
		up = ov.Rate.Upload
		down = ov.Rate.Download
	}
	activeStr := fmt.Sprintf("%d", active)
	if st.Running {
		m.status.SetTitle(fmt.Sprintf("↓ %s  ↑ %s", trayRate(down), trayRate(up)))
		// The exit address rides along when a self-check has seen one: it is
		// the answer to "is this traffic actually leaving from where the
		// rules promise", visible without opening the window at all.
		tooltipVars := map[string]string{
			"down": trayRate(down), "up": trayRate(up), "n": activeStr,
		}
		tooltipKey := "tray.tooltipRate"
		if r := a.GetLastProbe(); r != nil && r.ExitIP != "" {
			tooltipVars["ip"] = r.ExitIP
			tooltipKey = "tray.tooltipExit"
		}
		systray.SetTooltip(text(tooltipKey, tooltipVars))
	} else {
		m.status.SetTitle(text("tray.stopped", nil))
		systray.SetTooltip(text("tray.tooltipStopped", nil))
	}
	m.conns.SetTitle(text("tray.conns", map[string]string{"n": activeStr}))

	m.show.SetTitle(text("tray.show", nil))
	m.start.SetTitle(text("tray.start", nil))
	m.stop.SetTitle(text("tray.stop", nil))
	m.mode.SetTitle(text("tray.mode", nil))
	m.rule.SetTitle(text("tray.modeRule", nil))
	m.global.SetTitle(text("tray.modeGlobal", nil))
	m.direct.SetTitle(text("tray.modeDirect", nil))
	m.quit.SetTitle(text("tray.quit", nil))

	if st.SystemProxy {
		m.sysProxy.SetTitle(text("tray.systemProxyOn", nil))
	} else {
		m.sysProxy.SetTitle(text("tray.systemProxyOff", nil))
	}
	switch {
	case st.Tun:
		m.tun.SetTitle(text("tray.tunOn", nil))
	case st.TunError != "":
		m.tun.SetTitle(text("tray.tunFailed", nil))
	default:
		m.tun.SetTitle(text("tray.tunOff", nil))
	}
	applyModeChecks(st.ProxyMode, m)
}

// trayRate is the compact form used in the tooltip: one decimal is noise at a
// glance, so rates get integer units.
func trayRate(n int64) string {
	if n <= 0 {
		return "0B/s"
	}
	units := []string{"B/s", "K/s", "M/s", "G/s"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d%s", int64(v), units[i])
	}
	return fmt.Sprintf("%.1f%s", v, units[i])
}

// showMainWindow raises the window from the tray. Unminimising is not enough
// for a window that was closed to the tray (that one is hidden, not minimised),
// and Windows will not hand the foreground to a process that is not already in
// front of the user, so the always-on-top round trip is what actually pulls it
// above whatever had focus.
func showMainWindow(a *App) {
	ctx := a.ctx
	if ctx == nil {
		return
	}
	wailsruntime.WindowUnminimise(ctx)
	wailsruntime.WindowShow(ctx)
	wailsruntime.WindowSetAlwaysOnTop(ctx, true)
	wailsruntime.WindowSetAlwaysOnTop(ctx, false)
}

// trayIcon returns icon bytes in the format the platform expects: Windows
// wants an ICO container, everything else takes PNG directly.
func trayIcon() []byte {
	if goruntime.GOOS == "windows" {
		return icoFromPNG(appIconPNG)
	}
	return appIconPNG
}

// icoFromPNG wraps a PNG into the single-image ICO container format. Windows
// supports PNG-compressed entries since Vista; a width byte of 0 means 256px
// and Windows scales it down for the tray.
func icoFromPNG(png []byte) []byte {
	const headerSize = 6 + 16 // ICONDIR + one ICONDIRENTRY
	buf := make([]byte, headerSize+len(png))
	buf[2] = 1                                  // ICONDIR.type: 1 = icon
	buf[4] = 1                                  // ICONDIR.count: 1 image
	binary.LittleEndian.PutUint16(buf[10:], 1)  // planes
	binary.LittleEndian.PutUint16(buf[12:], 32) // bits per pixel
	binary.LittleEndian.PutUint32(buf[14:], uint32(len(png)))
	binary.LittleEndian.PutUint32(buf[18:], headerSize)
	copy(buf[headerSize:], png)
	return buf
}

// systrayQuit removes the tray icon; called from App.shutdown when the app
// really exits.
func systrayQuit() {
	systray.Quit()
}
