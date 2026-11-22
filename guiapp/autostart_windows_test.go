//go:build windows

package guiapp

import "testing"

// TestLaunchAtLoginRoundTrip exercises the real HKCU Run key: enable, verify,
// disable, verify. t.Cleanup removes the entry even on failure, so a crashed
// run cannot leave an autostart pointing at the test binary.
func TestLaunchAtLoginRoundTrip(t *testing.T) {
	t.Cleanup(func() { _ = disableLaunchAtLogin() })

	if launchAtLoginEnabled() {
		// A previous crashed run or a real installation already registered
		// the value name; start from a clean slate.
		if err := disableLaunchAtLogin(); err != nil {
			t.Fatalf("pre-clean: %v", err)
		}
	}

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
// the intent is off.
func TestSyncLaunchAtLogin(t *testing.T) {
	t.Cleanup(func() { _ = disableLaunchAtLogin() })
	if err := disableLaunchAtLogin(); err != nil {
		t.Fatalf("pre-clean: %v", err)
	}

	// Intent on, entry absent: sync registers it.
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
