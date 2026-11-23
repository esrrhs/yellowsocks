//go:build windows

package guiapp

import "testing"

// TestLaunchAtLoginRoundTrip exercises the real HKCU Run key: enable, verify,
// disable, verify. The entry is restored rather than dropped on the way out —
// these tests run against the live registry, where the value name also belongs
// to the installed copy of the app, so a blind delete would quietly turn the
// user's autostart off.
func TestLaunchAtLoginRoundTrip(t *testing.T) {
	prev := launchAtLoginRaw()
	t.Cleanup(func() { restoreLaunchAtLoginRaw(prev) })

	if err := enableLaunchAtLogin(); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !launchAtLoginEnabled() {
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

	if err := disableLaunchAtLogin(); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if launchAtLoginEnabled() {
		t.Fatal("entry still present after disable")
	}
}

// TestSyncLaunchAtLogin checks the repair paths: a missing entry is
// re-registered when the intent is on, and a foreign entry is cleared when
// the intent is off. Like the round trip, it leaves the registry as it found it.
func TestSyncLaunchAtLogin(t *testing.T) {
	prev := launchAtLoginRaw()
	t.Cleanup(func() { restoreLaunchAtLoginRaw(prev) })

	// Intent on, entry absent: sync registers it.
	if err := disableLaunchAtLogin(); err != nil {
		t.Fatalf("pre-clean: %v", err)
	}
	syncLaunchAtLogin(guiSettings{LaunchAtLogin: true})
	if !launchAtLoginEnabled() {
		t.Fatal("sync did not register with intent on")
	}

	// Intent off, entry present: sync removes it.
	syncLaunchAtLogin(guiSettings{LaunchAtLogin: false})
	if launchAtLoginEnabled() {
		t.Fatal("sync did not remove with intent off")
	}
}
