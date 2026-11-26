//go:build windows

package guiapp

import "testing"

// TestLaunchAtLoginRoundTrip exercises the real HKCU Run key: enable, verify,
// disable, verify. The entry is restored rather than dropped on the way out —
// these tests run against the live registry, where the value name also belongs
// to the installed copy of the app, so a blind delete would quietly turn the
// user's autostart off.
//
// Only the Run-key helpers are called. enableLaunchAtLogin would register a
// logon task whenever the test happens to run elevated, and deleting that task
// afterwards is a change to the machine, not a test.
func TestLaunchAtLoginRoundTrip(t *testing.T) {
	prev := launchAtLoginRaw()
	t.Cleanup(func() { restoreLaunchAtLoginRaw(prev) })

	if err := enableRunEntry(); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if launchAtLoginRaw() == "" {
		t.Fatal("entry missing after enable")
	}
	cmd := launchAtLoginRaw()
	if cmd == "" {
		t.Fatal("registered command is empty")
	}
	// The registered command must start the app hidden (tray only at boot).
	if cmd[len(cmd)-len(" --hidden"):] != " --hidden" {
		t.Fatalf("registered command %q does not start hidden", cmd)
	}

	if err := deleteRunEntry(); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if launchAtLoginRaw() != "" {
		t.Fatal("entry still present after disable")
	}
}

// TestSyncLaunchAtLogin checks the Run-key repair path: a missing entry is
// re-registered when the intent is on and no logon task covers it, and a stale
// entry is cleared when the intent is off. Like the round trip, it leaves the
// registry as it found it and never touches the scheduled task.
func TestSyncLaunchAtLogin(t *testing.T) {
	// GitHub Actions Windows runners (and any admin session) are elevated.
	// syncLaunchAtLogin then registers a logon task and deletes the Run key,
	// so a check of the Run key would fail and leave a leftover task. This
	// test only exercises the unelevated Run-key repair path.
	if isElevated() {
		t.Skip("this process is elevated; syncLaunchAtLogin uses the logon task, not the Run key")
	}
	if launchTaskRegistered() {
		t.Skip("a logon task is registered on this machine; the Run-key path is superseded")
	}
	prev := launchAtLoginRaw()
	t.Cleanup(func() { restoreLaunchAtLoginRaw(prev) })

	// Intent on, entry absent: sync registers it.
	if err := deleteRunEntry(); err != nil {
		t.Fatalf("pre-clean: %v", err)
	}
	syncLaunchAtLogin(guiSettings{LaunchAtLogin: true})
	if launchAtLoginRaw() == "" {
		t.Fatal("sync did not register with intent on")
	}

	// Intent off, entry present: sync removes it.
	syncLaunchAtLogin(guiSettings{LaunchAtLogin: false})
	if launchAtLoginRaw() != "" {
		t.Fatal("sync did not remove with intent off")
	}
}
