package launch

import (
	"os"
	"path/filepath"
)

// EnsureWritableWorkDir makes the process working directory writable.
// loggo creates its log files in the working directory and panics when that
// open fails. Returns false when no writable directory could be selected;
// callers should then disable log files instead of letting loggo panic.
func EnsureWritableWorkDir() bool {
	if cwd, err := os.Getwd(); err == nil && dirWritable(cwd) {
		return true
	}
	var candidates []string
	if d, err := os.UserConfigDir(); err == nil && d != "" {
		candidates = append(candidates, filepath.Join(d, "YellowSocks"))
	}
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		candidates = append(candidates, filepath.Join(d, "YellowSocks"))
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		candidates = append(candidates, filepath.Join(h, "Documents", "YellowSocks"))
	}
	if d := os.TempDir(); d != "" {
		candidates = append(candidates, filepath.Join(d, "YellowSocks"))
	}
	for _, dir := range candidates {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			continue
		}
		if !dirWritable(dir) {
			continue
		}
		if err := os.Chdir(dir); err == nil {
			return true
		}
	}
	return false
}

func dirWritable(dir string) bool {
	probe := filepath.Join(dir, ".yellowsocks-write-test")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	f.Close()
	_ = os.Remove(probe)
	return true
}
