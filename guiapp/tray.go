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

		// Status lines: disabled items used as read-only text. Two of them
		// keep the icon useful without opening the window.
		mStatus := systray.AddMenuItem("代理已停止", "Current transfer rate")
		mStatus.Disable()
		mConns := systray.AddMenuItem("连接: 0", "Active connections")
		mConns.Disable()
		systray.AddSeparator()

		mShow := systray.AddMenuItem("显示主界面", "Show the YellowSocks window")
		systray.AddSeparator()
		mStart := systray.AddMenuItem("启动代理", "Start the proxy engine")
		mStop := systray.AddMenuItem("停止代理", "Stop the proxy engine")
		mStop.Disable()
		mSysProxy := systray.AddMenuItem("系统代理: 关", "Toggle the OS-wide system proxy")
		mTun := systray.AddMenuItem("TUN 模式: 关", "Toggle TUN mode (needs administrator)")

		mMode := systray.AddMenuItem("分流模式", "Routing mode")
		mRule := mMode.AddSubMenuItemCheckbox("规则", "Domestic direct, foreign proxied", true)
		mGlobal := mMode.AddSubMenuItemCheckbox("全局", "Everything through the proxy", false)
		mDirect := mMode.AddSubMenuItemCheckbox("直连", "Everything direct", false)

		systray.AddSeparator()
		mQuit := systray.AddMenuItem("退出", "Quit YellowSocks")

		// A left click on the icon is the reflex for "open it again" once the
		// window has been closed to the tray, and on Windows it did nothing
		// at all before: only the menu item was wired. The menu keeps its own
		// entry as the discoverable equivalent, and the right click still
		// opens the menu because only the left button is claimed here.
		systray.SetOnClick(func(systray.IMenu) { showMainWindow(a) })
		mShow.Click(func() { showMainWindow(a) })
		mStart.Click(func() {
			go func() {
				_ = a.StartEngine()
				refreshTray(a, mStatus, mConns, mStart, mStop, mSysProxy, mTun, mRule, mGlobal, mDirect)
			}()
		})
		mStop.Click(func() {
			go func() {
				_ = a.StopEngine()
				refreshTray(a, mStatus, mConns, mStart, mStop, mSysProxy, mTun, mRule, mGlobal, mDirect)
			}()
		})
		mSysProxy.Click(func() {
			go func() {
				_ = a.SetSystemProxy(!a.GetSystemProxy())
				refreshTray(a, mStatus, mConns, mStart, mStop, mSysProxy, mTun, mRule, mGlobal, mDirect)
			}()
		})
		mTun.Click(func() {
			// SetTunMode can block for the UAC handover; never do that on the
			// menu's own goroutine, which also serves the window messages.
			go func() {
				_ = a.SetTunMode(!a.GetTunMode())
				refreshTray(a, mStatus, mConns, mStart, mStop, mSysProxy, mTun, mRule, mGlobal, mDirect)
			}()
		})
		mRule.Click(func() { setModeFromTray(a, "rule", mRule, mGlobal, mDirect) })
		mGlobal.Click(func() { setModeFromTray(a, "global", mRule, mGlobal, mDirect) })
		mDirect.Click(func() { setModeFromTray(a, "direct", mRule, mGlobal, mDirect) })
		mQuit.Click(a.quitApp)

		refreshTray(a, mStatus, mConns, mStart, mStop, mSysProxy, mTun, mRule, mGlobal, mDirect)

		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					refreshTray(a, mStatus, mConns, mStart, mStop, mSysProxy, mTun, mRule, mGlobal, mDirect)
				case <-stop:
					return
				}
			}
		}()
	}, func() { close(stop) })
}

// setModeFromTray applies a mode and moves the checkmark, reading the mode back
// afterwards instead of trusting the click: the backend normalises unknown
// values and is the only thing that knows what actually took effect.
func setModeFromTray(a *App, mode string, mRule, mGlobal, mDirect *systray.MenuItem) {
	go func() {
		if err := a.SetProxyMode(mode); err != nil {
			return
		}
		applyModeChecks(a.GetProxyMode(), mRule, mGlobal, mDirect)
	}()
}

func applyModeChecks(mode string, mRule, mGlobal, mDirect *systray.MenuItem) {
	if mode == "global" {
		mGlobal.Check()
	} else {
		mGlobal.Uncheck()
	}
	if mode == "direct" {
		mDirect.Check()
	} else {
		mDirect.Uncheck()
	}
	if mode == "rule" {
		mRule.Check()
	} else {
		mRule.Uncheck()
	}
}

// refreshTray pulls one snapshot and rewrites the menu titles. Everything is
// read through the App methods, so the tray cannot drift from the window.
func refreshTray(a *App, mStatus, mConns, mStart, mStop, mSysProxy, mTun, mRule, mGlobal, mDirect *systray.MenuItem) {
	defer func() {
		// The menu is destroyed at exit while this may still be in flight;
		// a panic here would take down the shutdown path.
		_ = recover()
	}()

	st := a.EngineStatus()
	if st.Running {
		mStart.Disable()
		mStop.Enable()
	} else {
		mStart.Enable()
		mStop.Disable()
	}

	active := 0
	up, down := int64(0), int64(0)
	if ov := a.GetOverview(); ov != nil {
		active = ov.ActiveConns
		up = ov.Rate.Upload
		down = ov.Rate.Download
	}
	if st.Running {
		mStatus.SetTitle(fmt.Sprintf("↓ %s  ↑ %s", trayRate(down), trayRate(up)))
		systray.SetTooltip(fmt.Sprintf("YellowSocks · ↓ %s ↑ %s · %d 连接", trayRate(down), trayRate(up), active))
	} else {
		mStatus.SetTitle("代理已停止")
		systray.SetTooltip("YellowSocks · 已停止")
	}
	mConns.SetTitle(fmt.Sprintf("连接: %d", active))

	if st.SystemProxy {
		mSysProxy.SetTitle("系统代理: 开")
	} else {
		mSysProxy.SetTitle("系统代理: 关")
	}
	switch {
	case st.Tun:
		mTun.SetTitle("TUN 模式: 开")
	case st.TunError != "":
		mTun.SetTitle("TUN 模式: 失败")
	default:
		mTun.SetTitle("TUN 模式: 关")
	}
	applyModeChecks(st.ProxyMode, mRule, mGlobal, mDirect)
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
