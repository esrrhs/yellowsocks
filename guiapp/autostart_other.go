//go:build !windows

package guiapp

import "errors"

// Launch-at-login is implemented per platform. Only Windows ships it right
// now; other shells keep the bindings but report the feature as unavailable
// instead of silently pretending to register.

func launchAtLoginEnabled() bool { return false }

func enableLaunchAtLogin() error {
	return errors.New("launch at login is not supported on this platform yet")
}

func disableLaunchAtLogin() error { return nil }

func syncLaunchAtLogin(st guiSettings) {}
