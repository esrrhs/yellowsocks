package version

import (
	"encoding/json"
	"fmt"
	"runtime"
)

var (
	// Version holds the current semantic version of YellowSocks.
	// Injected at compile time via -ldflags "-X ...Version=x.y.z"
	Version = "1.1.0"

	// GitCommit holds the git commit sha.
	// Injected at compile time via -ldflags "-X ...GitCommit=abc1234"
	GitCommit = "unknown"

	// BuildTime holds the build timestamp.
	// Injected at compile time via -ldflags "-X ...BuildTime=YYYY-MM-DD HH:MM:SS"
	BuildTime = "unknown"
)

// Info represents full build and version metadata
type Info struct {
	Version   string `json:"version"`
	GitCommit string `json:"git_commit"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// GetInfo returns structured version information
func GetInfo() Info {
	return Info{
		Version:   Version,
		GitCommit: GitCommit,
		BuildTime: BuildTime,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}

// String returns formatted human-readable version string
func String() string {
	return fmt.Sprintf("YellowSocks %s (commit: %s, built at: %s, %s/%s, %s)",
		Version, GitCommit, BuildTime, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// JSON returns version information as JSON string
func JSON() string {
	data, _ := json.Marshal(GetInfo())
	return string(data)
}
