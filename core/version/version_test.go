package version

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVersionInfo(t *testing.T) {
	info := GetInfo()
	if info.Version == "" {
		t.Errorf("expected non-empty version")
	}
	if info.GoVersion == "" {
		t.Errorf("expected non-empty GoVersion")
	}
	if info.OS == "" || info.Arch == "" {
		t.Errorf("expected non-empty OS and Arch")
	}

	str := String()
	if !strings.Contains(str, "YellowSocks") {
		t.Errorf("expected version string to contain YellowSocks, got %s", str)
	}

	jsonStr := JSON()
	var parsed Info
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		t.Fatalf("failed to unmarshal version JSON: %v", err)
	}
	if parsed.Version != info.Version {
		t.Errorf("expected parsed version %s, got %s", info.Version, parsed.Version)
	}
}

// The About page shows this verbatim, so the exact shape matters: seven
// characters like git's short sha, and a marker when the tree had edits.
func TestShortenRevision(t *testing.T) {
	cases := []struct {
		rev   string
		dirty bool
		want  string
	}{
		{"9098818114bd149e8a287284e9191cfa484e2b25", false, "9098818"},
		{"9098818114bd149e8a287284e9191cfa484e2b25", true, "9098818-dirty"},
		{"abc1234", false, "abc1234"},
		{"abc", false, "abc"},
	}
	for _, c := range cases {
		if got := shortenRevision(c.rev, c.dirty); got != c.want {
			t.Errorf("shortenRevision(%q, %v) = %q, want %q", c.rev, c.dirty, got, c.want)
		}
	}
}

// A build must never surface the placeholder when a real value is available:
// the GUI's own build path (wails) omits ldflags, so this is the fallback.
func TestBuildMetadataIsFilled(t *testing.T) {
	if GitCommit == "unknown" && commitFromBuildInfo() != "unknown" {
		t.Errorf("commit available from build info but not used")
	}
	if BuildTime == "unknown" {
		t.Errorf("BuildTime still unknown; the executable timestamp should have filled it")
	}
}
