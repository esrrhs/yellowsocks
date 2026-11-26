package version

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
)

var (
	// Version holds the current semantic version of YellowSocks.
	// Injected at compile time via -ldflags "-X ...Version=x.y.z"
	Version = "2.0.0"

	// GitCommit holds the git commit sha.
	// Injected at compile time via -ldflags "-X ...GitCommit=abc1234"
	GitCommit = "unknown"

	// BuildTime holds the build timestamp.
	// Injected at compile time via -ldflags "-X ...BuildTime=YYYY-MM-DD HH:MM:SS"
	BuildTime = "unknown"
)

// init fills the two fields that ldflags normally provides, so a build that
// forgot to pass them still shows something true instead of "unknown".
//
// The GUI is built by `wails build`, which hardcodes -buildvcs=false: the VCS
// stamp that `go build` would embed is not there, so the commit is usually
// only available through ldflags (see windows/build.sh). BuildTime falls back
// to the executable's own timestamp, which is the moment it was linked.
func init() {
	if GitCommit == "unknown" {
		GitCommit = commitFromBuildInfo()
	}
	if BuildTime == "unknown" {
		BuildTime = buildTimeFromSelf()
	}
}

func commitFromBuildInfo() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return GitCommit
	}
	rev := ""
	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return GitCommit
	}
	return shortenRevision(rev, dirty)
}

// shortenRevision keeps the first seven characters the way git prints a short
// sha, and marks an uncommitted tree: a "-dirty" build is not the commit it
// claims to be, and the report should say so.
func shortenRevision(rev string, dirty bool) string {
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}

func buildTimeFromSelf() string {
	exe, err := os.Executable()
	if err != nil {
		return BuildTime
	}
	st, err := os.Stat(exe)
	if err != nil {
		return BuildTime
	}
	return st.ModTime().Format("2006-01-02 15:04:05")
}

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
