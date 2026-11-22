package main

import (
	"embed"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/yellowsocks/guiapp"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

// macOS shell for the YellowSocks GUI. All shared logic (engine bindings,
// tray, system proxy, embedded frontend assets) lives in guiapp; this file
// only wires the Wails app for macOS. Build with `wails build` in this
// directory.
func main() {
	// Keep logs in a writable place (the app bundle directory is read-only
	// for signed installs, so this falls back to the per-user directory).
	// loggo writes into the CWD and panics when it cannot, so this must run
	// before logging starts.
	logWritable, startHidden := guiapp.Boot()
	loggo.Ini(loggo.Config{
		Level:     loggo.NameToLevel("info"),
		Prefix:    "yellowsocks-gui",
		NoLogFile: !logWritable,
	})

	// A previous instance may have been hard-killed while the system proxy
	// was registered; clear that stale registration so traffic is not sent
	// to a dead port (which would break networking for every app on the
	// machine, this one included).
	guiapp.CleanupStaleSystemProxy()

	app := guiapp.NewApp()
	app.EnableFrontendEvents()

	err := wails.Run(&options.App{
		Title:     "YellowSocks",
		Width:     1024,
		Height:    720,
		MinWidth:  800,
		MinHeight: 600,
		StartHidden: startHidden,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 24, G: 26, B: 32, A: 1},
		OnStartup:        app.Startup,
		OnBeforeClose:    app.BeforeClose,
		OnShutdown:       app.Shutdown,
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		loggo.Error("fatal: %s", err.Error())
	}
}
