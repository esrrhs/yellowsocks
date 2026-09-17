//go:build darwin

package main

import (
	"fmt"
	"os/exec"
	"time"

	"github.com/getlantern/systray"
)

// RunNativeWindow on macOS opens the dashboard in the default browser and
// then hands control to the systray event loop (which must run on the main
// thread on macOS via NSApplicationMain).
func RunNativeWindow() {
	// Open the web dashboard in the system browser after a short pause so the
	// HTTP server has time to bind its port.
	go func() {
		time.Sleep(500 * time.Millisecond)
		url := fmt.Sprintf("http://127.0.0.1:%d", appCfg.WebPort)
		if err := exec.Command("open", url).Run(); err != nil {
			// Non-fatal — user can open the URL manually from the tray menu.
			_ = err
		}
	}()

	// systray.Run blocks until systray.Quit() is called.  On macOS this call
	// MUST happen on the main goroutine (it drives the NSRunLoop internally).
	systray.Run(onReady, onExit)
}
