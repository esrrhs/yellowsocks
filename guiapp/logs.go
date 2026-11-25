package guiapp

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Log viewing. loggo writes one file per level per day
// (<prefix>_<LEVEL>_<date>.log), so "the log" is really four files that have to
// be merged to look like the single stream a user expects.
//
// The GUI reads the tail on demand instead of keeping a subscriber: the log is
// already on disk, and a file read cannot desynchronise from it or miss lines
// written while the view was closed.

const (
	guiLogPrefix = "yellowsocks-gui"
	// guiLogTailBytes bounds how much of each file is read per request. A line
	// is a few hundred bytes, so this covers well over the ~200 rows the view
	// asks for while keeping a 1s refresh cheap on a multi-megabyte file.
	guiLogTailBytes = 96 * 1024
)

// guiLogLevels is the merge order when the caller asks for everything. loggo
// only creates a file once something is written at that level, so missing
// files are normal and not an error.
var guiLogLevels = []string{"INFO", "WARN", "ERROR", "DEBUG"}

// GetLogs returns the most recent log lines, oldest first.
//
// level selects one loggo file ("INFO", "WARN", "ERROR", "DEBUG") or ""/"ALL"
// for the merged stream. At most limit lines are returned; a non-positive or
// oversized limit is clamped rather than rejected, because the view passes a
// user-selected size and a bad value should not blank the page.
func (a *App) GetLogs(level string, limit int) []string {
	if limit <= 0 || limit > 2000 {
		limit = 300
	}
	lv := strings.ToUpper(strings.TrimSpace(level))
	targets := []string{lv}
	if lv == "" || lv == "ALL" {
		targets = guiLogLevels
	}

	date := time.Now().Format("2006-01-02")
	var all []string
	for _, name := range targets {
		path := filepath.Join(DataDir(), guiLogPrefix+"_"+name+"_"+date+".log")
		lines, err := tailLines(path, guiLogTailBytes, limit)
		if err != nil {
			continue
		}
		all = append(all, lines...)
	}
	if len(targets) > 1 {
		// The prefix is an ISO-8601 timestamp, so lexicographic order is
		// chronological order and no parsing is needed.
		sort.SliceStable(all, func(i, j int) bool { return logTimeKey(all[i]) < logTimeKey(all[j]) })
		if len(all) > limit {
			all = all[len(all)-limit:]
		}
	}
	return all
}

// GetLogPath reports where the log files live, so the UI can point a user at
// them instead of describing the path in prose.
func (a *App) GetLogPath() string { return DataDir() }

// tailLines reads the last maxBytes of path and returns up to limit complete
// lines from it. Reading from an offset rather than streaming the whole file
// keeps the cost flat as the file grows.
func tailLines(path string, maxBytes int64, limit int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	start := int64(0)
	if size > maxBytes {
		start = size - maxBytes
	}
	buf := make([]byte, size-start)
	if len(buf) == 0 {
		return nil, nil
	}
	if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
		return nil, err
	}

	lines := strings.Split(string(buf), "\n")
	// When the read started mid-file the first element is a partial line.
	if start > 0 && len(lines) > 1 {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines, nil
}

// logTimeKey extracts the bracketed timestamp from a loggo line, e.g.
// "[INFO] [2026-11-25T21:43:08.8090209+08:00] [file.go:12] ...".
func logTimeKey(line string) string {
	first := strings.Index(line, "[")
	if first < 0 {
		return ""
	}
	second := strings.Index(line[first+1:], "[")
	if second < 0 {
		return ""
	}
	rest := line[first+1+second+1:]
	end := strings.Index(rest, "]")
	if end <= 0 {
		return ""
	}
	return rest[:end]
}
