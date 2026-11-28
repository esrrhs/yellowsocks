//go:build !windows

package guiapp

// The mutual-restart guard is built on Windows named mutexes and shipped only
// with the Windows app. These stubs keep the shared Startup/tray path
// compiling on macOS and Linux.

func clearWatchdogStop() {}

func stopWatchdog() {}

func writeInstanceState() {}

func (a *App) watchdogLoop() {}
