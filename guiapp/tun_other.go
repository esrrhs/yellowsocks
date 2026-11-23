//go:build !windows

package guiapp

import "errors"

// TUN mode is Windows-only right now: the shells for other platforms keep
// the bindings so the frontend works everywhere, and report the feature as
// unavailable instead of silently pretending to enable it.

func tunEnable(params TunParams) error {
	return errors.New("TUN mode is not supported on this platform yet")
}

func tunDisable() error { return nil }

func tunIsActive() bool { return false }

func tunSessionStatsLive() string { return "unsupported" }
