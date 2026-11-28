//go:build windows

package guiapp

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/esrrhs/gohome/loggo"
	"golang.org/x/sys/windows"
)

var (
	procEnumWindows    = moduser32.NewProc("EnumWindows")
	procGetWindowPID   = moduser32.NewProc("GetWindowThreadProcessId")
	procGetClassNameW  = moduser32.NewProc("GetClassNameW")
	procIsWindow       = moduser32.NewProc("IsWindow")
	procSendMsgTimeout = moduser32.NewProc("SendMessageTimeoutW")
)

const (
	systrayWindowClass = "SystrayClass"
	wmNull             = 0
	smtoAbortIfHung    = 2
	trayPingTimeoutMS  = 2000
)

// watchTrayLoop proves, every few seconds, that the tray window's message loop
// still answers. The failure it watches for is silent: the icon itself is
// drawn by Explorer, so it stays on screen even when the thread that owns it
// stops pumping messages — clicks and the menu then go nowhere while the rest
// of the app looks perfectly healthy. A goroutine dump taken at that moment
// names whatever the tray thread is stuck on; without it the hang is a
// mystery that repeats itself.
func watchTrayLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	var (
		hwnd      uintptr
		hungSince time.Time // first failed probe of the current episode
		warned    bool      // the episode lasted long enough to log
		dumped    bool
	)
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		if hwnd == 0 || !isWindow(hwnd) {
			hwnd = findTrayWindow()
		}
		if hwnd == 0 {
			continue
		}
		if trayResponds(hwnd) {
			if warned {
				loggo.Warn("[GUI] tray watchdog: message loop answers again after %s", time.Since(hungSince).Round(time.Second))
			}
			hungSince, warned, dumped = time.Time{}, false, false
			continue
		}
		if hungSince.IsZero() {
			// One missed probe happens under load (a busy shell, icon churn);
			// only a miss that repeats is the failure worth reporting.
			hungSince = time.Now()
			continue
		}
		if !warned && time.Since(hungSince) >= 5*time.Second {
			warned = true
			loggo.Warn("[GUI] tray watchdog: tray window stopped answering %s ago; clicks and the menu are dead until this clears",
				time.Since(hungSince).Round(time.Second))
		}
		if !dumped && time.Since(hungSince) >= 10*time.Second {
			dumped = true
			if path := dumpTrayHangStacks(); path != "" {
				loggo.Warn("[GUI] tray watchdog: still silent after %s; goroutine stacks written to %s",
					time.Since(hungSince).Round(time.Second), path)
			}
		}
	}
}

// findTrayWindow locates the hidden window energye/systray creates for this
// process. The class name is generic — other apps built on the same systray
// fork use it too — so the process id decides which window is ours; with the
// single-instance mutex held there is at most one.
//
// The enumeration callback is registered once: syscall.NewCallback has a hard
// per-process cap, and the watchdog may probe for hours.
var (
	trayEnumOnce  sync.Once
	trayEnumProc  uintptr
	trayEnumFound uintptr
	trayEnumSelf  = windows.GetCurrentProcessId()
)

func findTrayWindow() uintptr {
	trayEnumOnce.Do(func() {
		trayEnumProc = syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
			var pid uint32
			procGetWindowPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
			if pid != trayEnumSelf {
				return 1 // continue enumeration
			}
			var buf [64]uint16
			n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
			if n == 0 || windows.UTF16ToString(buf[:n]) != systrayWindowClass {
				return 1
			}
			trayEnumFound = hwnd
			return 0 // stop enumeration
		})
	})
	trayEnumFound = 0
	procEnumWindows.Call(trayEnumProc, 0)
	return trayEnumFound
}

// trayResponds sends WM_NULL and waits for the owning thread to pick it up.
// A thread that is pumping messages answers within microseconds; a blocked
// one answers never, which is exactly the failure being watched for.
func trayResponds(hwnd uintptr) bool {
	var res uintptr
	r1, _, _ := procSendMsgTimeout.Call(hwnd, wmNull, 0, 0, smtoAbortIfHung, trayPingTimeoutMS, uintptr(unsafe.Pointer(&res)))
	return r1 != 0
}

func isWindow(hwnd uintptr) bool {
	r1, _, _ := procIsWindow.Call(hwnd)
	return r1 != 0
}

// dumpTrayHangStacks writes every goroutine stack to a file next to the data
// files. The dump is kept whole — the tray thread's own stack is the headline,
// but the goroutine it is waiting on (or the one holding a lock it needs) is
// just as likely to be the answer, and filtering here would hide it.
func dumpTrayHangStacks() string {
	var buf []byte
	for size := 1 << 20; size <= 16<<20; size *= 2 {
		buf = make([]byte, size)
		if n := runtime.Stack(buf, true); n < size {
			buf = buf[:n]
			break
		}
	}
	path := filepath.Join(DataDir(), "tray-hang-"+time.Now().Format("20060102-150405")+".log")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		loggo.Warn("[GUI] tray watchdog: writing stack dump: %v", err)
		return ""
	}
	return path
}
