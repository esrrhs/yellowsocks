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

// Held for the life of the process; the kernel drops it on exit, including a
// crash, which is why a mutex beats a PID or lock file here.
var singleInstanceHandle windows.Handle

var (
	modkernel32      = windows.NewLazySystemDLL("kernel32.dll")
	procCreateMutexW = modkernel32.NewProc("CreateMutexW")
	procSetLastError = modkernel32.NewProc("SetLastError")
)

// createMutex wraps CreateMutexW and keeps its raw last error. x/sys's
// windows.CreateMutex cannot be used here: it only reports an error when no
// handle came back, and hides ERROR_ALREADY_EXISTS -- the one bit that says
// "somebody else owns this name". With that wrapper the check silently passed
// for every process, so a second copy ran alongside the first.
func createMutex(name *uint16) (windows.Handle, error) {
	procSetLastError.Call(0)
	r0, _, err := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	return windows.Handle(r0), err
}

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
	waited := false
	for {
		handle, cerr := createMutex(name)
		switch {
		case handle == 0:
			// Cannot tell (unusual access error): do not block a legitimate
			// start over it.
			loggo.Warn("[GUI] single-instance mutex: %v", cerr)
			return true
		case cerr == nil || !errors.Is(cerr, windows.ERROR_ALREADY_EXISTS):
			singleInstanceHandle = handle
			if waited {
				loggo.Info("[GUI] single-instance acquired after waiting for the predecessor")
			}
			return true
		}
		// The name already existed: this handle refers to the live mutex, so
		// drop it and keep polling against the owner's next move.
		_ = windows.CloseHandle(handle)
		if time.Now().After(deadline) {
			loggo.Info("[GUI] another instance is already running; bringing its window forward")
			focusExistingWindow()
			return false
		}
		waited = true
		time.Sleep(200 * time.Millisecond)
	}
}

// ReleaseSingleInstance gives up the claim early. The TUN handover quits the
// process right after the elevated successor reports in, and that successor is
// blocked on this name: releasing here turns an 8s wait into an instant one,
// and removes the window where the successor times out and exits while the old
// copy is still tearing TUN down.
func ReleaseSingleInstance() {
	if singleInstanceHandle != 0 {
		_ = windows.CloseHandle(singleInstanceHandle)
		singleInstanceHandle = 0
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
