package guiapp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/esrrhs/gohome/loggo"
)

// RestartRequestName is the drop-a-file trigger. While the app runs it polls
// its own data directory, and a file with this name makes it hand over to a
// fresh copy of itself -- which is how a rebuild gets rolled out without asking
// anyone to quit from the tray first. The file is consumed before the handover,
// so it cannot loop.
const RestartRequestName = "restart.request"

// RestartRequestPath is the full path to drop the trigger at.
func RestartRequestPath() string { return filepath.Join(DataDir(), RestartRequestName) }

// Two seconds is slow enough to cost nothing and fast enough that the person
// waiting on a rebuild does not notice the lag.
const restartPollInterval = 2 * time.Second

// restartWatcher polls for the trigger file for the life of the process.
func (a *App) restartWatcher() {
	path := RestartRequestPath()
	ticker := time.NewTicker(restartPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.shutdownDone:
			return
		case <-ticker.C:
			if _, err := os.Stat(path); err != nil {
				continue
			}
			// Consume the request first: the replacement reads the same
			// directory and would otherwise restart forever.
			if err := os.Remove(path); err != nil {
				loggo.Warn("[GUI] restart request is not removable: %s", err.Error())
				continue
			}
			loggo.Info("[GUI] restart requested (%s); starting a replacement copy", RestartRequestName)
			a.restartForUpgrade()
			return
		}
	}
}

// restartForUpgrade starts a replacement copy and shuts this one down.
//
// The replacement is spawned as a plain child, so it inherits our elevation and
// no UAC appears; it also cannot bind the engine ports yet, which is exactly
// what --restarted tells it to wait out. Our own flags are passed through
// except --hidden: a restart is an explicit act, so it should come back with a
// window rather than silently landing in the tray.
func (a *App) restartForUpgrade() {
	exe, err := os.Executable()
	if err != nil {
		loggo.Error("[GUI] restart: cannot locate our own exe: %s", err.Error())
		return
	}
	args := make([]string, 0, len(os.Args))
	for _, arg := range os.Args[1:] {
		if isHiddenFlag(arg) || isRestartedFlag(arg) {
			continue
		}
		args = append(args, arg)
	}
	args = append(args, restartedFlag)
	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	if err := cmd.Start(); err != nil {
		loggo.Error("[GUI] restart: could not start the replacement copy: %s", err.Error())
		return
	}
	loggo.Info("[GUI] restart: replacement copy started (pid %d), shutting down", cmd.Process.Pid)
	a.armForcedExit("restart")
	a.quitApp()
}

// armForcedExit guarantees the process goes away even if teardown hangs. The
// successor is already blocking on the instance claim by then, and two copies
// fighting over routes is worse than an abrupt exit.
func (a *App) armForcedExit(reason string) {
	go func() {
		select {
		case <-a.shutdownDone:
			// Graceful path finished; main unwinds on its own.
			return
		case <-time.After(20 * time.Second):
			loggo.Warn("[GUI] %s: teardown did not finish in time; exiting anyway", reason)
			os.Exit(0)
		}
	}()
}

func isHiddenFlag(arg string) bool {
	switch strings.ToLower(arg) {
	case "--hidden", "-hidden", "/hidden":
		return true
	}
	return false
}

func isRestartedFlag(arg string) bool {
	switch strings.ToLower(arg) {
	case restartedFlag, "-restarted", "/restarted":
		return true
	}
	return false
}
