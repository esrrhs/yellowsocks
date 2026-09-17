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
