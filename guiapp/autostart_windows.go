//go:build windows

package guiapp

import (
	"fmt"
	"os"

	"github.com/esrrhs/gohome/loggo"
	"golang.org/x/sys/windows/registry"
)

// Launch-at-login via the per-user Run key. It needs no admin rights, works
// for portable exes anywhere on disk and is removed again on disable. The
// registered command starts the app with --hidden so a boot login brings up
// the tray only; the engine auto-connects and restores the system proxy on
// its own (settings.json).

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

// launchAtLoginEnabled reports whether our Run-key entry exists.
func launchAtLoginEnabled() bool {
	return launchAtLoginRaw() != ""
}

func enableLaunchAtLogin() error {
	cmd, err := launchAtLoginCommand()
	if err != nil {
		return err
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
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

func disableLaunchAtLogin() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
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

// syncLaunchAtLogin repairs the Run-key entry at app startup: re-register the
// current exe path when it moved, and clear an entry left behind when the
// persisted intent was turned off elsewhere.
func syncLaunchAtLogin(st guiSettings) {
	current := launchAtLoginRaw()
	if st.LaunchAtLogin {
		desired, err := launchAtLoginCommand()
		if err == nil && current != desired {
			_ = enableLaunchAtLogin()
		}
		return
	}
	if current != "" {
		_ = disableLaunchAtLogin()
	}
}
