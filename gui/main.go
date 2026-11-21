package main

import (
	"embed"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/yellowsocks/core/launch"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// loggo writes log files into the CWD and panics when it cannot, so make
	// sure we are in a writable place before initializing logging. This is the
	// same dance the CLI does (see cli/main.go).
	loggo.Ini(loggo.Config{
		Level:     loggo.NameToLevel("info"),
		Prefix:    "yellowsocks-gui",
		NoLogFile: !launch.EnsureWritableWorkDir(),
	})

	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "YellowSocks",
		Width:     1024,
		Height:    720,
		MinWidth:  800,
		MinHeight: 600,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 24, G: 26, B: 32, A: 1},
		OnStartup:        app.startup,
		OnBeforeClose:    app.beforeClose,
		OnShutdown:       app.shutdown,
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		loggo.Error("fatal: %s", err.Error())
	}
}
