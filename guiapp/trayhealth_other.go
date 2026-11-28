//go:build !windows

package guiapp

// The tray health watchdog pings a Win32 message-only window; on the other
// platforms the systray implementation has no equivalent handle to probe.
func watchTrayLoop(<-chan struct{}) {}
