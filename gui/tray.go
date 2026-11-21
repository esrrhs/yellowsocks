package main

import (
	_ "embed"
	"encoding/binary"
	goruntime "runtime"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/energye/systray"
)

//go:embed build/appicon.png
var appIconPNG []byte

// runTray populates and serves the system tray menu. It runs in its own
// goroutine started by App.startup. energye/systray is the community fork
// designed to coexist with Wails' event loop.
func runTray(a *App) {
	systray.Run(func() {
		systray.SetIcon(trayIcon())
		systray.SetTooltip("YellowSocks")

		mShow := systray.AddMenuItem("显示主界面", "Show the YellowSocks window")
		systray.AddSeparator()
		mStart := systray.AddMenuItem("启动代理", "Start the proxy engine")
		mStop := systray.AddMenuItem("停止代理", "Stop the proxy engine")
		mStop.Disable()
		mSysProxy := systray.AddMenuItem("系统代理: 关", "Toggle the OS-wide system proxy")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("退出", "Quit YellowSocks")

		// Reflect the current intent on startup.
		if a.GetSystemProxy() {
			mSysProxy.SetTitle("系统代理: 开")
		}

		mShow.Click(func() {
			wailsruntime.WindowShow(a.ctx)
		})
		mStart.Click(func() {
			if err := a.StartEngine(); err == nil {
				mStart.Disable()
				mStop.Enable()
			}
		})
		mStop.Click(func() {
			if err := a.StopEngine(); err == nil {
				mStop.Disable()
				mStart.Enable()
			}
		})
		mSysProxy.Click(func() {
			next := !a.GetSystemProxy()
			if err := a.SetSystemProxy(next); err == nil {
				if next {
					mSysProxy.SetTitle("系统代理: 开")
				} else {
					mSysProxy.SetTitle("系统代理: 关")
				}
			}
		})
		mQuit.Click(a.quitApp)
	}, func() {})
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
