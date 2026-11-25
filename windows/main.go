package main

import (
	"embed"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/yellowsocks/guiapp"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

// Windows shell for the YellowSocks GUI. All shared logic (engine bindings,
// tray, system proxy, embedded frontend assets) lives in guiapp; this file
// only wires the Wails app for Windows. Build with `wails build` in this
// directory.
func main() {
	// Portable mode: keep logs next to the exe (or the per-user fallback
	// when that is not writable) and adopt any per-user data into a fresh
	// portable directory. loggo writes into the CWD and panics when it
	// cannot, so this must run before logging starts.
	logWritable, startHidden := guiapp.Boot()
	loggo.Ini(loggo.Config{
		Level:     loggo.NameToLevel("info"),
		Prefix:    "yellowsocks-gui",
		NoLogFile: !logWritable,
	})

	// A previous instance may have been hard-killed while the system proxy
	// was registered; clear that stale registration before WebView2 starts,
	// because a proxy pointing at a dead port would block the WebView2
	// subprocess itself from launching.
	guiapp.CleanupStaleSystemProxy()

	// The TUN-mode elevated relaunch hands over to this copy: give the old
	// instance a moment to release its listen ports, then confirm we are
	// alive so the old instance can quit. The logon task starts us elevated
	// with no predecessor, so it opts out of both waits.
	//
	// This has to run before the single-instance claim below: the old copy is
	// still alive precisely until it sees the handshake, so claiming first
	// would wait out the whole timeout and quit.
	if guiapp.ElevatedRequested() && !guiapp.ElevatedDirect() {
		time.Sleep(2 * time.Second)
		guiapp.WriteElevatedHandshake()
	}

	// Only one copy may run: a second one cannot bind the engine ports and
	// would tear down the live TUN session's routes on its way out. Two
	// timeouts, because the wait means different things: a plain start just
	// wants the running copy's window brought forward (be quick about it),
	// while a handover successor genuinely has to outlast the predecessor's
	// TUN teardown, which is much slower than an empty shutdown.
	instanceWait := time.Second
	if (guiapp.ElevatedRequested() && !guiapp.ElevatedDirect()) || guiapp.Restarted() {
		instanceWait = 25 * time.Second
	}
	if !guiapp.AcquireSingleInstance(instanceWait) {
		return
	}

	app := guiapp.NewApp()
	app.EnableFrontendEvents()

	err := wails.Run(&options.App{
		Title:     "YellowSocks",
		Width:     1024,
		Height:    720,
		MinWidth:  800,
		MinHeight: 600,
		// Launch-at-login starts the app with --hidden: tray only, the
		// engine and system proxy come up on their own.
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
