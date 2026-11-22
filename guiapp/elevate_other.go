//go:build !windows

package guiapp

import "errors"

func isElevated() bool { return false }

func relaunchElevated() error {
	return errors.New("TUN mode is not supported on this platform yet")
}
