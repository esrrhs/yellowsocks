//go:build windows

package guiapp

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/wailsapp/wails/v2/pkg/runtime"
	"golang.org/x/sys/windows"
)

// The mutual-restart pair: this side keeps the guard process alive, the guard
// (repo: watchdog/) restarts this app when its single-instance mutex vanishes.
// The watchdog binary hard-codes the same names; the two modules cannot share
// a package, so both copies carry this cross-reference.
const (
	watchdogExeName   = "yellowsocks-watchdog.exe"
	watchdogMutexName = `Local\YellowSocksWatchdog`
	// watchdogStopName means "both processes go down": the tray quit and the
	// update handover drop it, and the app itself exits on sight of one so a
	// marker written from outside takes the whole pair down as well. Only the
	// TUN elevation handover leaves it alone -- the same binaries keep running,
	// so the guard has to survive the swap.
	watchdogStopName = "watchdog.stop"
	// instanceStateName publishes {"pid":..,"elevated":..}; the guard uses it
	// to hand over to an elevated copy of itself, because only that copy can
	// restart an elevated app without a UAC prompt.
	instanceStateName = "instance.state"

	watchdogPollInterval = 5 * time.Second
)

type instanceState struct {
	PID      int  `json:"pid"`
	Elevated bool `json:"elevated"`
}

func watchdogStopPath() string { return filepath.Join(DataDir(), watchdogStopName) }

// clearWatchdogStop removes the marker a previous quit or update handover left
// behind. Runs before the guard is (re)started, otherwise a fresh app launch
// would find a guard that exits on sight and respawn it in a loop.
func clearWatchdogStop() {
	if err := os.Remove(watchdogStopPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		loggo.Warn("[GUI] cannot clear the watchdog stop marker: %s", err.Error())
	}
}

// stopWatchdog tells the guard to exit instead of restarting us: the tray quit
// and the update handover both use it, because both take the whole pair down.
func stopWatchdog() {
	if err := os.WriteFile(watchdogStopPath(), []byte("quit\n"), 0o644); err != nil {
		loggo.Warn("[GUI] cannot write the watchdog stop marker: %s", err.Error())
	}
}

// quitOnWatchdogStopMarker turns a marker this app did not just write into a
// clean shutdown. Someone dropping the file (an updater, a script) means the
// pair is supposed to go down; a lone app would otherwise knock on a dismissed
// guard every five seconds. Markers from our own quit paths are already covered
// by requestQuit returning false, since those paths set the flag before writing
// the file.
func (a *App) quitOnWatchdogStopMarker() bool {
	if _, err := os.Stat(watchdogStopPath()); err != nil {
		return false
	}
	if !a.requestQuit() {
		return false
	}
	loggo.Info("[GUI] watchdog stop marker present; both processes exit")
	_ = a.StopEngine()
	runtime.Quit(a.ctx)
	return true
}

func writeInstanceState() {
	data, err := json.Marshal(instanceState{PID: os.Getpid(), Elevated: isElevated()})
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(DataDir(), instanceStateName), data, 0o644); err != nil {
		loggo.Warn("[GUI] cannot publish the instance state: %s", err.Error())
	}
}

// mutexHeld reports whether a live process owns the named mutex. The probe
// creates and closes a handle of its own: when nobody else holds the name, it
// is the only owner for the moment, so it cannot keep anything alive.
func mutexHeld(name string) bool {
	ptr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false
	}
	handle, cerr := createMutex(ptr)
	if handle == 0 {
		return false
	}
	_ = windows.CloseHandle(handle)
	return cerr != nil && errors.Is(cerr, windows.ERROR_ALREADY_EXISTS)
}

// startWatchdog spawns the guard as a plain child, so it inherits our
// elevation -- the guard must be at least as privileged as the app it
// restarts.
func startWatchdog() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	dir := filepath.Dir(exe)
	guard := filepath.Join(dir, watchdogExeName)
	args := []string{"--app", exe}
	if StartHiddenRequested() {
		// A tray-only start should come back tray-only: the guard inherits
		// the flag so a restart does not pop a window nobody asked for.
		args = append(args, "--hidden")
	}
	cmd := exec.Command(guard, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	loggo.Info("[GUI] watchdog started (pid %d)", pid)
	return nil
}

// watchdogLoop keeps the guard process alive for the life of the app.
func (a *App) watchdogLoop() {
	warned := false
	ensure := func() bool {
		a.mu.Lock()
		quitting := a.quitting
		a.mu.Unlock()
		if quitting {
			return false
		}
		if mutexHeld(watchdogMutexName) {
			warned = false
			return true
		}
		if err := startWatchdog(); err != nil {
			if !warned {
				// Once per outage, not once per tick: a missing or blocked
				// watchdog binary should not flood the log.
				loggo.Warn("[GUI] watchdog unavailable: %s", err.Error())
				warned = true
			}
		} else {
			warned = false
		}
		return true
	}
	if a.quitOnWatchdogStopMarker() || !ensure() {
		return
	}
	ticker := time.NewTicker(watchdogPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.shutdownDone:
			return
		case <-ticker.C:
			if a.quitOnWatchdogStopMarker() || !ensure() {
				return
			}
		}
	}
}
