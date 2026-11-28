//go:build windows

package guiapp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestMutexHeldProbe(t *testing.T) {
	name := fmt.Sprintf(`Local\YellowSocksTestMutex%d`, os.Getpid())
	if mutexHeld(name) {
		t.Fatal("an unclaimed mutex was reported as held")
	}
	ptr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	handle, cerr := createMutex(ptr)
	if handle == 0 {
		t.Fatalf("createMutex: %v", cerr)
	}
	defer windows.CloseHandle(handle)
	if !mutexHeld(name) {
		t.Fatal("a claimed mutex was reported as free")
	}
}

func TestWatchdogStopMarker(t *testing.T) {
	clearWatchdogStop()
	if _, err := os.Stat(watchdogStopPath()); err == nil {
		t.Fatal("stop marker still present after clear")
	}
	stopWatchdog()
	if _, err := os.Stat(watchdogStopPath()); err != nil {
		t.Fatalf("stop marker missing after stop: %v", err)
	}
	clearWatchdogStop()
	if _, err := os.Stat(watchdogStopPath()); err == nil {
		t.Fatal("stop marker still present after the second clear")
	}
}

func TestRequestQuitOnlyOnce(t *testing.T) {
	a := NewApp()
	if !a.requestQuit() {
		t.Fatal("the first quit request must be accepted")
	}
	if a.requestQuit() {
		t.Fatal("a second quit request must be refused")
	}
	if !a.quitting {
		t.Fatal("quitting flag not set")
	}
}

func TestQuitOnWatchdogStopMarkerAbsent(t *testing.T) {
	clearWatchdogStop()
	a := NewApp()
	if a.quitOnWatchdogStopMarker() {
		t.Fatal("no marker on disk, the app must keep running")
	}
	if a.quitting {
		t.Fatal("quitting flag set without a marker")
	}
}

func TestWriteInstanceState(t *testing.T) {
	writeInstanceState()
	data, err := os.ReadFile(filepath.Join(DataDir(), instanceStateName))
	if err != nil {
		t.Fatalf("instance state not written: %v", err)
	}
	var st instanceState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("instance state is not JSON: %v", err)
	}
	if st.PID != os.Getpid() {
		t.Fatalf("state pid = %d, want %d", st.PID, os.Getpid())
	}
	if st.Elevated != isElevated() {
		t.Fatalf("state elevated = %v, want %v", st.Elevated, isElevated())
	}
}
