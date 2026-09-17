//go:build darwin

package main

/*
#cgo darwin CFLAGS: -DDARWIN -x objective-c -fobjc-arc
#cgo darwin LDFLAGS: -framework Cocoa -framework WebKit

#include "window_darwin.h"
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"time"
	"unsafe"

	"github.com/getlantern/systray"
)

// RunNativeWindow on macOS sets up the native Cocoa application, registers the
// systray callbacks, and presents the native macOS WebKit window.
func RunNativeWindow() {
	// Register systray callbacks without starting an extra run loop
	systray.Register(onReady, onExit)

	// Automatically open the native UI window once the local HTTP API is running
	go func() {
		time.Sleep(300 * time.Millisecond)
		url := fmt.Sprintf("http://127.0.0.1:%d", appCfg.WebPort)
		openControlPanel(url)
	}()

	// Run Cocoa NSApplication main loop on the main thread
	cTitle := C.CString("YellowSocks - Control Panel")
	defer C.free(unsafe.Pointer(cTitle))
	C.configureAppWindow(cTitle, C.int(960), C.int(680))
}

// openControlPanel brings the native macOS WebKit window to the foreground and loads the URL.
func openControlPanel(url string) {
	cUrl := C.CString(url)
	defer C.free(unsafe.Pointer(cUrl))
	C.showAppWindow(cUrl)
}
