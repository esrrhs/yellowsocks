//go:build !windows

package guiapp

import "os/exec"

// hiddenCommand exists so the shared call sites compile everywhere; only
// Windows gives a child process a console window of its own.
func hiddenCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}
