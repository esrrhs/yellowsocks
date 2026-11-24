//go:build windows

package guiapp

import (
	"os/exec"
	"syscall"
)

// createNoWindow tells the loader not to give the child a console at all.
// HideWindow alone only asks for SW_HIDE on a console that still exists, which
// is enough for a flash-free start but still allocates one; combining both is
// what keeps a console program completely off screen.
const createNoWindow = 0x08000000

// hiddenCommand builds a helper subprocess that never shows a window.
//
// The GUI is a windowsgui binary and owns no console, so every console tool it
// spawns (ipconfig, powershell) gets a brand new console window for as long as
// it runs. A single TUN bring-up runs several of them back to back — neighbour
// entry, resolver flush, DNS client restart, another flush — so at login the
// desktop showed a string of black boxes appearing and disappearing.
func hiddenCommand(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	return cmd
}
