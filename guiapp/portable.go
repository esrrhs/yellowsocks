package guiapp

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/yellowsocks/core/launch"
)

// Portable ("green") layout: when the executable's directory is writable,
// every data file (config.yaml, settings.json, stats.json) lives next to the
// exe, so the single binary can be copied anywhere and keeps its state with
// it. When the directory is read-only (Program Files, a signed macOS bundle,
// a read-only USB stick) the per-user config directory is used instead.
//
// Existing installations keep working: a fresh portable directory adopts the
// data files still found in the per-user directory (one-time migration).

var (
	dataDirOnce sync.Once
	dataDirPath string
)

// DataDir returns the directory that holds config.yaml, settings.json and
// stats.json. The location is resolved once per process.
func DataDir() string {
	dataDirOnce.Do(func() {
		dataDirPath = resolveDataDir()
	})
	return dataDirPath
}

func resolveDataDir() string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if dirWritable(dir) {
			return dir
		}
	}
	return userConfigDataDir()
}

// userConfigDataDir is the fallback location, shared in style with the CLI
// (e.g. %APPDATA%/YellowSocks on Windows).
func userConfigDataDir() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "YellowSocks")
	}
	return "."
}

func dirWritable(dir string) bool {
	probe := filepath.Join(dir, ".yellowsocks-write-test")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	f.Close()
	_ = os.Remove(probe)
	return true
}

// AdoptUserData copies config.yaml, settings.json and stats.json from the
// per-user directory into a fresh portable directory, so an upgrade from an
// older build does not lose the configured upstream or the traffic totals.
// Existing files are never overwritten.
func AdoptUserData() {
	portable := DataDir()
	if portable == userConfigDataDir() {
		return
	}
	for _, name := range []string{configName, settingsName, statsName} {
		dst := filepath.Join(portable, name)
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		src := filepath.Join(userConfigDataDir(), name)
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			continue
		}
		loggo.Info("[GUI] adopted %s from %s", name, userConfigDataDir())
	}
}

// Boot prepares the process for the GUI: it picks the working directory
// (portable exe directory when writable) and adopts per-user data files into
// a fresh portable directory. It reports whether logging to files is possible
// and whether the app should start hidden (--hidden, set by the
// launch-at-login registration). In -tags bindings builds (the wailsbindings
// binding-dump tool, whose exe lives in a temp directory) Boot is a no-op so
// the generator keeps running in the project directory.
func Boot() (logWritable bool, startHidden bool) {
	if bindingsMode {
		return true, false
	}
	logWritable = PrepareWorkDir()
	AdoptUserData()
	return logWritable, StartHiddenRequested()
}

// PrepareWorkDir selects a writable working directory for the process. In
// portable mode the exe directory is used, so log files land next to the exe
// together with the data files. Returns false when nothing is writable;
// callers should then disable log files instead of letting loggo panic.
func PrepareWorkDir() bool {
	if dir := DataDir(); dirWritable(dir) {
		if err := os.Chdir(dir); err == nil {
			return true
		}
	}
	return launch.EnsureWritableWorkDir()
}

// StartHiddenRequested reports whether the app was launched with --hidden,
// which is how the launch-at-login registration starts it: no window, just
// the tray, from where the proxy and system proxy come up per settings.
func StartHiddenRequested() bool {
	return hasArg("--hidden", "-hidden", "/hidden")
}

// ElevatedRequested reports whether the app was launched with --elevated,
// which the TUN-mode elevated relaunch passes to its own copy: the new
// instance waits briefly for the old instance's ports to close.
func ElevatedRequested() bool {
	return hasArg("--elevated", "-elevated", "/elevated")
}

// ElevatedDirect reports whether this copy was started by the logon scheduled
// task, which runs at the highest privilege already. There is no predecessor
// to hand over from in that case, so the two waits that exist only to make a
// UAC relaunch safe (the port-release sleep and the handshake) are skipped.
func ElevatedDirect() bool {
	return hasArg("--elevated-direct", "-elevated-direct", "/elevated-direct")
}

// The flag a replacement copy gets after a restart request: it means "your
// predecessor is still tearing down, wait for the instance claim".
const restartedFlag = "--restarted"

// Restarted reports whether this copy was started to replace a running one.
func Restarted() bool { return hasArg(restartedFlag, "-restarted", "/restarted") }

func hasArg(want ...string) bool {
	for _, arg := range os.Args[1:] {
		lower := strings.ToLower(arg)
		for _, w := range want {
			if lower == w {
				return true
			}
		}
	}
	return false
}

// Elevated-relaunch handshake: the relaunched elevated copy writes a marker
// file the instant it starts; the original instance polls for it and only
// quits once the successor is confirmed alive. If the user declines the UAC
// prompt, the marker never appears and the original stays up with an error.

const handshakeName = "tun-elevated-handshake"

func handshakePath() string { return filepath.Join(DataDir(), handshakeName) }

// WriteElevatedHandshake marks that the elevated copy started. Called from
// main() of the relaunched process.
func WriteElevatedHandshake() {
	path := handshakePath()
	_ = os.Remove(path)
	now := time.Now()
	_ = os.WriteFile(path, []byte(now.Format(time.RFC3339Nano)), 0o644)
}

// ClearElevatedHandshake drops a stale marker so a fresh launch is not
// confused with one from an older run.
func ClearElevatedHandshake() { _ = os.Remove(handshakePath()) }

// WaitElevatedHandshake waits up to timeout for the elevated copy to start.
// Reports whether it showed up.
func WaitElevatedHandshake(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, err := os.Stat(handshakePath()); err == nil && time.Since(st.ModTime()) <= timeout {
			_ = os.Remove(handshakePath())
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// WaitForPortFree blocks until a TCP address can be bound again (the previous
// instance exiting) or the timeout elapses.
func WaitForPortFree(addr string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			_ = ln.Close()
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}
