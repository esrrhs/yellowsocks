// Command yellowsocks-watchdog is the guard half of the GUI's mutual-restart
// pair: it lives next to yellowsocks-gui.exe and restarts it whenever it
// disappears. The GUI does the same for the watchdog, so whichever process
// dies, the other brings it back.
//
// windows/build.sh builds this binary into windows/build/bin alongside the
// GUI; the shared contract (mutex names, marker files) is documented in
// run_windows.go and mirrored in guiapp/watchdog_windows.go.
package main

import "os"

func main() {
	os.Exit(run())
}
