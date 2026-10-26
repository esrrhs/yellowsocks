package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Save without a configured file is an error, and so is Load.
func TestSaveLoadWithoutFile(t *testing.T) {
	c := newTestCollector()
	if err := c.Save(); err == nil || !strings.Contains(err.Error(), "no stats file configured") {
		t.Fatalf("Save without file = %v, want config error", err)
	}
	if err := c.Load(); err == nil || !strings.Contains(err.Error(), "no stats file configured") {
		t.Fatalf("Load without file = %v, want config error", err)
	}
}

// A stats path below a regular file cannot have its directory created.
func TestSaveMkdirAllError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	c := New(Config{HistoryMinutes: 5, BucketInterval: time.Second, File: filepath.Join(blocker, "stats.json")})
	err := c.Save()
	if err == nil || !strings.Contains(err.Error(), "create stats dir") {
		t.Fatalf("Save under a file = %v, want mkdir error", err)
	}
}

// A stale directory occupying the .tmp path makes the atomic write fail.
func TestSaveWriteTmpError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "stats.json")
	if err := os.MkdirAll(file+".tmp", 0o755); err != nil {
		t.Fatalf("mkdir tmp blocker: %v", err)
	}
	c := New(Config{HistoryMinutes: 5, BucketInterval: time.Second, File: file})
	err := c.Save()
	if err == nil || !strings.Contains(err.Error(), "write stats tmp") {
		t.Fatalf("Save over a tmp directory = %v, want write error", err)
	}
}

// The final rename fails when the destination is a directory; the leftover
// .tmp file must be removed. With the background saver enabled, both the
// periodic save and the final save on Close log the failure and keep going.
func TestSaveRenameError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "stats.json")
	if err := os.MkdirAll(file, 0o755); err != nil {
		t.Fatalf("mkdir dest blocker: %v", err)
	}
	c := New(Config{HistoryMinutes: 5, BucketInterval: time.Second, File: file, SaveInterval: 20 * time.Millisecond})
	err := c.Save()
	if err == nil || !strings.Contains(err.Error(), "rename stats file") {
		t.Fatalf("Save onto a directory = %v, want rename error", err)
	}
	if _, err := os.Stat(file + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp file not removed after failed rename")
	}
	// Let the saver hit the failing save at least once, then close, which
	// attempts one more save. Neither may panic or block.
	time.Sleep(100 * time.Millisecond)
	c.Close()
}

// Load must fail loudly (so New can log and continue) on unreadable files,
// broken JSON and unknown versions.
func TestLoadErrorPaths(t *testing.T) {
	dir := t.TempDir()

	// Reading a directory fails with something other than NotExist.
	c := New(Config{HistoryMinutes: 5, BucketInterval: time.Second, File: dir})
	err := c.Load()
	if err == nil || !strings.Contains(err.Error(), "read stats file") {
		t.Fatalf("Load of a directory = %v, want read error", err)
	}

	// Broken JSON.
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := writeFile(corrupt, "{not json"); err != nil {
		t.Fatalf("write: %v", err)
	}
	c = New(Config{HistoryMinutes: 5, BucketInterval: time.Second, File: corrupt})
	err = c.Load()
	if err == nil || !strings.Contains(err.Error(), "parse stats file") {
		t.Fatalf("Load of corrupt JSON = %v, want parse error", err)
	}

	// Unknown snapshot version.
	wrongVersion := filepath.Join(dir, "version.json")
	if err := writeFile(wrongVersion, `{"version": 999}`); err != nil {
		t.Fatalf("write: %v", err)
	}
	c = New(Config{HistoryMinutes: 5, BucketInterval: time.Second, File: wrongVersion})
	err = c.Load()
	if err == nil || !strings.Contains(err.Error(), "stats file version 999, want 1") {
		t.Fatalf("Load of wrong version = %v, want version error", err)
	}
}

// Loading filters what the snapshot contains: stale or malformed buckets,
// empty domain and node names, oversized error logs; null DNS maps must not
// stay nil.
func TestLoadFiltersSnapshotContents(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "stats.json")

	now := time.Now().Unix()
	snap := snapshot{
		Version: snapshotVersion,
		Totals:  Counters{Upload: 100, Download: 200, Conns: 3},
		DNS: persistedDNS{
			Queries:    7,
			ByType:     nil, // must come back as an empty map
			ByUpstream: nil,
			TopDomains: []DomainStat{
				{Domain: "", Conns: 5}, // unnamed entries are dropped
				{Domain: "q.com", Conns: 2},
			},
		},
		Buckets: []Bucket{
			{T: 0, Upload: 1},          // invalid timestamp
			{T: now - 3600, Upload: 2}, // outside the history window
			{T: now, Upload: 3},        // four valid entries, the 2-slot ring
			{T: now, Upload: 4},        // keeps only the newest two
			{T: now, Upload: 5},
			{T: now, Upload: 6},
		},
		Domains: []DomainStat{
			{Domain: "", Upload: 9},
			{Domain: "keep.com", Upload: 11, Download: 22, Conns: 2, ProxyConns: 2},
		},
		Nodes: []NodeStat{
			{Name: ""},
			{Name: "node-1", Upload: 33, Conns: 1},
		},
		Errors: []ErrorEntry{
			{Message: "old-1"}, {Message: "old-2"}, {Message: "old-3"},
			{Message: "new-1"}, {Message: "new-2"},
		},
		PeakUp:   900,
		PeakDown: 1800,
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if err := writeFile(file, string(data)); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	// HistoryMinutes: 2 with one-second buckets caps the ring at two slots,
	// so the four valid buckets must keep only the newest two.
	c := New(Config{
		HistoryMinutes: 2,
		BucketInterval: time.Second,
		File:           file,
		SaveInterval:   0,
		DomainLimit:    10,
		ErrorLimit:     2,
	})

	if c.dns.ByType == nil || c.dns.ByUpstream == nil {
		t.Fatal("restored DNS maps must not be nil")
	}
	if len(c.dnsNames) != 1 || c.dnsNames["q.com"] == nil {
		t.Fatalf("restored dns names = %+v", c.dnsNames)
	}
	if got := len(c.buckets); got != 2 {
		t.Fatalf("restored %d buckets, want the 2 newest", got)
	}
	for i, want := range []int64{5, 6} {
		if c.buckets[i].T != now || c.buckets[i].Upload != want {
			t.Fatalf("bucket %d = %+v, want t=%d upload=%d", i, c.buckets[i], now, want)
		}
	}
	d := c.Domains(10, "bytes")
	if len(d) != 1 || d[0].Domain != "keep.com" || d[0].Upload != 11 || d[0].Download != 22 {
		t.Fatalf("restored domains = %+v", d)
	}
	n := c.Nodes()
	if len(n) != 1 || n[0].Name != "node-1" || n[0].Upload != 33 {
		t.Fatalf("restored nodes = %+v", n)
	}
	errs := c.Errors(100)
	if len(errs) != 2 || errs[0].Message != "new-1" || errs[1].Message != "new-2" {
		t.Fatalf("restored errors = %+v, want the 2 newest", errs)
	}
	ov := c.Overview()
	if ov.Totals.Upload != 100 || ov.Totals.Conns != 3 {
		t.Fatalf("restored totals = %+v", ov.Totals)
	}
	if ov.Peak.Upload != 900 || ov.Peak.Download != 1800 {
		t.Fatalf("restored peak = %+v", ov.Peak)
	}
	if got := c.DNSStats(5); got.Queries != 7 || len(got.TopDomains) != 1 || got.TopDomains[0].Domain != "q.com" {
		t.Fatalf("restored dns = %+v", got)
	}
}

// Close stops the background saver and writes a final snapshot; the saver
// itself also persists on its ticker.
func TestCloseWritesFinalSnapshot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "stats.json")

	c := New(Config{
		HistoryMinutes: 5,
		BucketInterval: time.Second,
		File:           file,
		SaveInterval:   20 * time.Millisecond,
	})
	s := c.Begin(ProtoHTTP, "1.2.3.4:1", "close.com", 80, DecisionDirect, "")
	s.AddUpload(4321)
	s.End(nil)

	// Give the background saver time to fire at least once.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(file); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.Close()
	c.Close() // idempotent

	if c.StartTime().IsZero() {
		t.Fatal("StartTime not set")
	}

	restored := New(Config{HistoryMinutes: 5, BucketInterval: time.Second, File: file, SaveInterval: 0})
	if got := restored.Overview().Totals.Upload; got != 4321 {
		t.Fatalf("restored upload after Close = %d, want 4321", got)
	}
}
