//go:build windows

package guiapp

import (
	"errors"
	"time"
	"unsafe"

	"github.com/esrrhs/gohome/loggo"
	"golang.org/x/sys/windows"
)

// One process at a time. There are now two ways to be started at logon (the
// logon task and, on a machine that has not upgraded yet, the Run key) and the
// user can always double-click the exe, so a second copy is a real possibility.
// Two copies do not merely duplicate a tray icon: the second one fails to bind
// the engine ports, and its TUN bring-up tears down routes and DNS that the
// first copy is relying on -- the machine loses its network.
//
// The mutex lives in Local\ because the copies that matter share a session.
const singleInstanceName = `Local\YellowSocksGUI`

// AcquireSingleInstance claims the mutex, waiting up to timeout for a
// predecessor to let go: the TUN handover relaunches the exe before the old copy
// has finished exiting, and that copy is the one that must win the wait.
// Returns false when another instance still owns it, after bringing its window
// forward so a double-click does not look like nothing happened.
func AcquireSingleInstance(timeout time.Duration) bool {
	name, err := windows.UTF16PtrFromString(singleInstanceName)
	if err != nil {
		return true
	}
	deadline := time.Now().Add(timeout)
	for {
		handle, cerr := windows.CreateMutex(nil, false, name)
		if cerr == nil {
			// Deliberately never closed: the handle has to outlive this call,
			// and the kernel releases it when the process ends.
			_ = handle
			return true
		}
		if !errors.Is(cerr, windows.ERROR_ALREADY_EXISTS) {
			// Cannot tell (unusual access error): do not block a legitimate
			// start over it.
			loggo.Warn("[GUI] single-instance mutex: %v", cerr)
			return true
		}
		if time.Now().After(deadline) {
			focusExistingWindow()
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procFindWindowW         = user32.NewProc("FindWindowW")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procIsIconic            = user32.NewProc("IsIconic")
)

const (
	swShow    = 5
	swRestore = 9
)

// focusExistingWindow asks the running copy to show itself. The main window
// title is the app title, which is how it is found without any IPC channel.
func focusExistingWindow() {
	title, err := windows.UTF16PtrFromString("YellowSocks")
	if err != nil {
		return
	}
	hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(title)))
	if hwnd == 0 {
		return
	}
	if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
		procShowWindow.Call(hwnd, swRestore)
	} else {
		procShowWindow.Call(hwnd, swShow)
	}
	procSetForegroundWindow.Call(hwnd)
}
