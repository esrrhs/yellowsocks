//go:build windows

package guiapp

import (
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

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

// relaunchElevated starts an elevated copy of this exe with --elevated and
// hands the TUN intent over through the persisted settings. The caller quits
// afterwards; the new instance sleeps briefly in main so the ports of this
// instance can close first.
func relaunchElevated() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exeDir := filepath.Dir(exe)
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	args, _ := windows.UTF16PtrFromString("--elevated")
	cwd, _ := windows.UTF16PtrFromString(exeDir)
	return windows.ShellExecute(0, verb, file, args, cwd, windows.SW_SHOWNORMAL)
}
