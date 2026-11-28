//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"github.com/esrrhs/gohome/loggo"
	"golang.org/x/sys/windows"
)

// The GUI hard-codes the same names in guiapp/watchdog_windows.go. The two
// sides cannot share a Go package (separate modules), so both copies carry a
// comment pointing at the other.
const (
	// appMutexName is the GUI's single-instance claim. It doubles as the
	// liveness probe: the kernel drops it the instant the app exits, crash
	// included.
	appMutexName = `Local\YellowSocksGUI`
	// watchdogMutexName is this process's own claim, so a second watchdog (a
	// relaunch racing a slow exit) bows out instead of doubling up.
	watchdogMutexName = `Local\YellowSocksWatchdog`
)

const (
	// stopFlagName is written by the tray's Quit before the app exits: a
	// deliberate quit must not be undone by a restart seconds later.
	stopFlagName = "watchdog.stop"
	// stateName is published by the app at Startup: {"pid":..,"elevated":..}.
	// An unelevated watchdog cannot restart an elevated GUI without a UAC
	// prompt, so it steps aside and lets the elevated app start its own.
	stateName  = "instance.state"
	appExeName = "yellowsocks-gui.exe"

	pollInterval = 2 * time.Second
	// spawnGrace is how long a just-started app has to claim its mutex before
	// the watchdog decides it did not come up. Generous on purpose: a cold
	// start on a slow disk or under an antivirus scan has taken close to 20s
	// to reach the claim, and spawning into that window only produces a
	// duplicate that loses the single-instance race and flashes a window.
	// Doubles on every failed attempt up to maxRetryDelay.
	spawnGrace    = 20 * time.Second
	maxRetryDelay = 60 * time.Second
)

type instanceState struct {
	PID      int  `json:"pid"`
	Elevated bool `json:"elevated"`
}

func run() int {
	appPath := flag.String("app", "", "path to yellowsocks-gui.exe (default: next to this exe)")
	hidden := flag.Bool("hidden", false, "restart the app with --hidden (tray only)")
	flag.Parse()

	exe, err := os.Executable()
	if err != nil {
		return 1
	}
	if *appPath == "" {
		*appPath = filepath.Join(filepath.Dir(exe), appExeName)
	}
	appDir := filepath.Dir(*appPath)
	dir := dataDir(appDir)
	// loggo writes into the CWD: keep the watchdog's logs next to the app's
	// (the portable layout the GUI itself uses).
	_ = os.Chdir(dir)
	loggo.Ini(loggo.Config{
		Level:     loggo.NameToLevel("info"),
		Prefix:    "yellowsocks-watchdog",
		NoLogFile: !dirWritable(dir),
	})

	handle, claimed, cerr := claimMutex(watchdogMutexName)
	if !claimed {
		if cerr != nil {
			loggo.Warn("[WATCHDOG] cannot claim the watchdog mutex: %v", cerr)
		} else {
			loggo.Info("[WATCHDOG] another watchdog is already running; exiting")
		}
		return 0
	}
	// Held for the life of the process: were this closed early, the name
	// would read as free and the app would keep spawning replacements.
	defer windows.CloseHandle(handle)
	loggo.Info("[WATCHDOG] running (pid %d, app %s)", os.Getpid(), *appPath)

	var appArgs []string
	if *hidden {
		appArgs = append(appArgs, "--hidden")
	}
	stopPath := filepath.Join(dir, stopFlagName)
	statePath := filepath.Join(dir, stateName)

	var (
		missing     int // consecutive polls with the app gone
		spawns      int // spawns since the app was last seen alive
		nextAttempt time.Time
		appWasUp    bool
	)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(stopPath); err == nil {
			loggo.Info("[WATCHDOG] stop marker present; exiting")
			return 0
		}
		if mutexHeld(appMutexName) {
			if !appWasUp {
				loggo.Info("[WATCHDOG] app is up")
				appWasUp = true
			}
			if st := readInstanceState(statePath); st.Elevated && !isElevated() {
				loggo.Info("[WATCHDOG] app runs elevated and this watchdog does not; exiting so the elevated copy takes over")
				return 0
			}
			missing, spawns, nextAttempt = 0, 0, time.Time{}
			<-ticker.C
			continue
		}
		if appWasUp {
			loggo.Warn("[WATCHDOG] app is gone; restarting it")
			appWasUp = false
		}
		missing++
		// Two consecutive sightings: a handover (TUN elevation, rebuild swap)
		// frees the mutex for milliseconds between the old and the new copy,
		// and a third copy spawned into that window would only flash a window
		// and exit.
		if missing < 2 || time.Now().Before(nextAttempt) {
			<-ticker.C
			continue
		}
		if err := spawnApp(*appPath, appArgs, appDir); err != nil {
			loggo.Warn("[WATCHDOG] restart failed: %s", err.Error())
		}
		spawns++
		delay := spawnGrace
		for i := 1; i < spawns && delay < maxRetryDelay; i++ {
			delay *= 2
		}
		if delay > maxRetryDelay {
			delay = maxRetryDelay
		}
		nextAttempt = time.Now().Add(delay)
		<-ticker.C
	}
}

// spawnApp starts the GUI as a plain child, so it inherits this process's
// elevation: an elevated watchdog restarts the elevated app without a UAC
// prompt, which is the whole point of keeping the pair together.
func spawnApp(path string, args []string, dir string) error {
	cmd := exec.Command(path, args...)
	cmd.Dir = dir
	// Both sides are windowsgui binaries, so no console would appear anyway;
	// hiding is explicit and free.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	loggo.Info("[WATCHDOG] started the app (pid %d)", pid)
	return nil
}

func readInstanceState(path string) instanceState {
	var st instanceState
	data, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	// A half-written file (the app truncates before writing) parses as the
	// zero state, which only means "do not upgrade this tick".
	_ = json.Unmarshal(data, &st)
	return st
}

// dataDir mirrors the GUI's portable layout (guiapp/portable.go): the app's
// own directory when writable, the per-user config directory otherwise.
func dataDir(appDir string) string {
	if dirWritable(appDir) {
		return appDir
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "YellowSocks")
	}
	return appDir
}

func dirWritable(dir string) bool {
	probe := filepath.Join(dir, ".yellowsocks-watchdog-write-test")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	f.Close()
	_ = os.Remove(probe)
	return true
}

// isElevated reports whether this process runs with administrator rights.
func isElevated() bool {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()
	var elevation uint32
	var retLen uint32
	if err := windows.GetTokenInformation(token, windows.TokenElevation, (*byte)(unsafe.Pointer(&elevation)), uint32(unsafe.Sizeof(elevation)), &retLen); err != nil {
		return false
	}
	return elevation != 0
}

var (
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	procCreateMutexW = kernel32.NewProc("CreateMutexW")
	procSetLastError = kernel32.NewProc("SetLastError")
)

// createMutex keeps the raw last error: x/sys's windows.CreateMutex hides
// ERROR_ALREADY_EXISTS, which is exactly the bit that says "somebody else owns
// this name". Same quirk as guiapp/createMutex.
func createMutex(name string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	procSetLastError.Call(0)
	r0, _, cerr := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(p)))
	return windows.Handle(r0), cerr
}

// claimMutex takes ownership of the named mutex for the life of the caller.
// The returned handle must stay open: closing it releases the name, and the
// next probe (the app ensuring a watchdog exists) would see free and spawn a
// duplicate.
func claimMutex(name string) (windows.Handle, bool, error) {
	handle, cerr := createMutex(name)
	if handle == 0 {
		return 0, false, cerr
	}
	if cerr != nil && errors.Is(cerr, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(handle)
		return 0, false, nil
	}
	return handle, true, nil
}

// mutexHeld reports whether a live process owns the named mutex. It creates
// and immediately closes a handle of its own; when nobody else holds the name,
// that handle is the only owner, so the probe cannot keep anything alive.
func mutexHeld(name string) bool {
	handle, cerr := createMutex(name)
	if handle == 0 {
		return false
	}
	_ = windows.CloseHandle(handle)
	return cerr != nil && errors.Is(cerr, windows.ERROR_ALREADY_EXISTS)
}
