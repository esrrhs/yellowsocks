//go:build windows

package guiapp

import (
	"testing"
)

const ciLaunchTaskName = "YellowSocksCITest"

func useTestLaunchTask(t *testing.T) {
	t.Helper()
	if !isElevated() {
		t.Skip("registering a logon task requires an elevated process")
	}
	prev := launchTaskName
	launchTaskName = ciLaunchTaskName
	_ = deleteLaunchTask()
	t.Cleanup(func() {
		_ = deleteLaunchTask()
		launchTaskName = prev
	})
}

func TestLaunchTaskRegisterAndDelete(t *testing.T) {
	useTestLaunchTask(t)

	if launchTaskRegistered() {
		t.Fatal("pre-clean left a test task registered")
	}
	if err := ensureLaunchTask(); err != nil {
		t.Fatalf("ensureLaunchTask: %v", err)
	}
	if !launchTaskRegistered() {
		t.Fatal("task missing after ensureLaunchTask")
	}
	// A second call must be a no-op when the exe path has not changed.
	if err := ensureLaunchTask(); err != nil {
		t.Fatalf("ensureLaunchTask again: %v", err)
	}
	if err := deleteLaunchTask(); err != nil {
		t.Fatalf("deleteLaunchTask: %v", err)
	}
	if launchTaskRegistered() {
		t.Fatal("task still present after delete")
	}
}

func TestSyncLaunchAtLoginElevatedUsesLogonTask(t *testing.T) {
	useTestLaunchTask(t)
	prevRun := launchAtLoginRaw()
	t.Cleanup(func() { restoreLaunchAtLoginRaw(prevRun) })

	// Start from a leftover Run key so we can check it is dropped.
	if err := enableRunEntry(); err != nil {
		t.Fatalf("enableRunEntry: %v", err)
	}

	syncLaunchAtLogin(guiSettings{LaunchAtLogin: true})
	if !launchTaskRegistered() {
		t.Fatal("elevated sync did not register the logon task")
	}
	if launchAtLoginRaw() != "" {
		t.Fatal("elevated sync left the Run key in place; both would start at logon")
	}

	syncLaunchAtLogin(guiSettings{LaunchAtLogin: false})
	if launchTaskRegistered() {
		t.Fatal("elevated sync did not remove the logon task when intent is off")
	}
	if launchAtLoginRaw() != "" {
		t.Fatal("elevated sync left a Run key after turning intent off")
	}
}

func TestSetLaunchAtLoginElevatedRoundTrip(t *testing.T) {
	useTestLaunchTask(t)
	useTempConfigDir(t)
	prevRun := launchAtLoginRaw()
	t.Cleanup(func() { restoreLaunchAtLoginRaw(prevRun) })

	a := NewApp()
	if err := a.SetLaunchAtLogin(true); err != nil {
		t.Fatalf("SetLaunchAtLogin(true): %v", err)
	}
	if !a.GetLaunchAtLogin() || !launchTaskRegistered() {
		t.Fatal("launch at login was not registered as a logon task")
	}
	if err := a.SetLaunchAtLogin(false); err != nil {
		t.Fatalf("SetLaunchAtLogin(false): %v", err)
	}
	if a.GetLaunchAtLogin() || launchTaskRegistered() || launchAtLoginRaw() != "" {
		t.Fatal("launch at login was not fully removed")
	}
}

func TestPSStringQuotesAndEscapes(t *testing.T) {
	if got := psString(`C:\App\yellowsocks-gui.exe`); got != `'C:\App\yellowsocks-gui.exe'` {
		t.Fatalf("plain path: %q", got)
	}
	if got := psString(`C:\O'Brien\app.exe`); got != `'C:\O''Brien\app.exe'` {
		t.Fatalf("escaped quote: %q", got)
	}
}
