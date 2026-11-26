//go:build windows

package guiapp

import (
	"os"
	"testing"
)

func TestIsElevatedOnGitHubActions(t *testing.T) {
	got := isElevated()
	if os.Getenv("GITHUB_ACTIONS") == "true" && !got {
		t.Fatal("Windows GitHub Actions runners are expected to be elevated")
	}
	// Calling isElevated must be safe on any Windows machine; the value
	// itself is only asserted in CI, where the logon-task tests also depend
	// on it. relaunchElevated (UAC / ShellExecute "runas") is not invoked
	// here: there is no one to accept the prompt.
	t.Logf("isElevated=%v", got)
}
