//go:build windows

package guiapp

import (
	"fmt"
	"os"

	"github.com/esrrhs/gohome/loggo"
	"golang.org/x/sys/windows/registry"
)

// Launch-at-login registration for the unelevated path: the per-user Run key.
// It needs no admin rights, works for portable exes anywhere on disk and is
// removed again on disable. The registered command starts the app with
// --hidden so a boot login brings up the tray only; the engine auto-connects
// and restores the system proxy on its own (settings.json).
//
// The elevated copy upgrades this to a logon scheduled task (launchtask_windows.go)
// so that TUN users neither click a UAC prompt nor pay for the handover at every
// boot. Both helpers below decide between the two; the Run-key operations stay
// separate so the tests can exercise them without touching the live task.

const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValueName = "YellowSocks"

// launchAtLoginCommand is the exact registry value for the current binary.
func launchAtLoginCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	// Quote the path: the exe may live in a directory with spaces.
	return `"` + exe + `" --hidden`, nil
}

func launchAtLoginRaw() string {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer key.Close()
	v, _, err := key.GetStringValue(runValueName)
	if err != nil {
		return ""
	}
	return v
}

// launchAtLoginEnabled reports whether either registration exists: the logon
// task, or the Run key the unelevated path uses.
func launchAtLoginEnabled() bool {
	return launchTaskRegistered() || launchAtLoginRaw() != ""
}

func enableRunEntry() error {
	cmd, err := launchAtLoginCommand()
	if err != nil {
		return err
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open run key: %w", err)
	}
	defer key.Close()
	if err := key.SetStringValue(runValueName, cmd); err != nil {
		return fmt.Errorf("set run value: %w", err)
	}
	loggo.Info("[GUI] registered launch at login: %s", cmd)
	return nil
}

func deleteRunEntry() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		if err == registry.ErrNotExist {
			return nil
		}
		return fmt.Errorf("open run key: %w", err)
	}
	defer key.Close()
	if err := key.DeleteValue(runValueName); err != nil {
		if err == registry.ErrNotExist {
			return nil
		}
		return fmt.Errorf("delete run value: %w", err)
	}
	loggo.Info("[GUI] removed launch-at-login entry")
	return nil
}

// enableLaunchAtLogin picks the best registration this process is allowed to
// write. Elevated: a logon task, which starts the app at full privilege with no
// UAC prompt, and the Run key goes away because keeping both would launch two
// instances at logon (the second would lose the listen ports and sit there
// broken). Unelevated: the Run key, as before.
func enableLaunchAtLogin() error {
	if isElevated() {
		if err := ensureLaunchTask(); err == nil {
			if derr := deleteRunEntry(); derr != nil {
				// Two registrations would race at logon; the task is the one
				// worth keeping, so retry the cleanup once and report it.
				loggo.Warn("[GUI] drop Run key after task registration: %v", derr)
			}
			loggo.Info("[GUI] registered launch at login as an elevated logon task")
			return nil
		} else {
			loggo.Warn("[GUI] logon task unavailable, using the Run key instead: %v", err)
		}
	}
	return enableRunEntry()
}

func disableLaunchAtLogin() error {
	taskErr := deleteLaunchTask()
	runErr := deleteRunEntry()
	if taskErr != nil {
		return taskErr
	}
	return runErr
}

// restoreLaunchAtLoginRaw puts the key back to whatever it held before a caller
// touched it. The tests exercise the real HKCU Run key, and a plain cleanup
// would delete the entry of the installation that happens to be registered on
// the machine running them, not just undo the test's own change.
func restoreLaunchAtLoginRaw(prev string) {
	if prev == "" {
		_ = deleteRunEntry()
		return
	}
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return
	}
	defer key.Close()
	_ = key.SetStringValue(runValueName, prev)
}

// syncLaunchAtLogin repairs the registration at app startup: re-register
// against the current exe path when it moved, upgrade to the logon task when
// this copy is elevated, and clear whatever is left when the persisted intent
// was turned off elsewhere.
func syncLaunchAtLogin(st guiSettings) {
	if !st.LaunchAtLogin {
		_ = disableLaunchAtLogin()
		return
	}
	if isElevated() {
		// Re-registering every start keeps the task pointing at the exe after
		// the portable folder is moved, and costs one short-lived schtasks.
		_ = enableLaunchAtLogin()
		return
	}
	// An earlier elevated run may already have registered the task. Writing the
	// Run key back now would start a second instance at the next logon, so the
	// task wins and the key is dropped.
	if launchTaskRegistered() {
		_ = deleteRunEntry()
		return
	}
	current := launchAtLoginRaw()
	desired, err := launchAtLoginCommand()
	if err == nil && current != desired {
		_ = enableRunEntry()
	}
}
