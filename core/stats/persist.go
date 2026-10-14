package stats

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// snapshotVersion bumps whenever the on-disk layout changes incompatibly. An
// unreadable or outdated file is ignored: statistics are never important
// enough to stop the proxy from starting.
const snapshotVersion = 1

// persistedDNS is the on-disk form of DNSStat (without derived fields).
type persistedDNS struct {
	Queries    int64            `json:"queries"`
	CacheHits  int64            `json:"cache_hits"`
	Failures   int64            `json:"failures"`
	Direct     int64            `json:"direct"`
	DoH        int64            `json:"doh"`
	ByType     map[string]int64 `json:"by_type"`
	ByUpstream map[string]int64 `json:"by_upstream"`
	TopDomains []DomainStat     `json:"top_domains"`
}

// snapshot is the persisted collector state.
type snapshot struct {
	Version  int          `json:"version"`
	SavedAt  time.Time    `json:"saved_at"`
	Totals   Counters     `json:"totals"`
	Direct   Counters     `json:"direct"`
	Proxy    Counters     `json:"proxy"`
	DNS      persistedDNS `json:"dns"`
	Buckets  []Bucket     `json:"buckets"`
	Domains  []DomainStat `json:"domains"`
	Nodes    []NodeStat   `json:"nodes"`
	Errors   []ErrorEntry `json:"errors"`
	PeakUp   int64        `json:"peak_upload_bps"`
	PeakDown int64        `json:"peak_download_bps"`
}

// Save writes the current state to Config.File atomically.
func (c *Collector) Save() error {
	path := c.cfg.File
	if path == "" {
		return fmt.Errorf("no stats file configured")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create stats dir: %w", err)
	}

	c.mu.Lock()
	snap := snapshot{
		Version: snapshotVersion,
		SavedAt: time.Now(),
		Totals:  c.totals,
		Direct:  c.direct,
		Proxy:   c.proxy,
		DNS: persistedDNS{
			Queries:    c.dns.Queries,
			CacheHits:  c.dns.CacheHits,
			Failures:   c.dns.Failures,
			Direct:     c.dns.Direct,
			DoH:        c.dns.DoH,
			ByType:     copyMap(c.dns.ByType),
			ByUpstream: copyMap(c.dns.ByUpstream),
			TopDomains: c.topFromLocked(c.dnsNames, c.cfg.DomainLimit),
		},
		Buckets:  c.orderedBucketsLocked(),
		Domains:  allDomainsLocked(c.domains),
		Nodes:    allNodesLocked(c.nodes),
		Errors:   c.errs,
		PeakUp:   c.peakUp,
		PeakDown: c.peakDown,
	}
	c.mu.Unlock()

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal stats: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write stats tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename stats file: %w", err)
	}
	return nil
}

// Load restores a snapshot written by Save. Buckets older than the
// configured history window are dropped.
func (c *Collector) Load() error {
	path := c.cfg.File
	if path == "" {
		return fmt.Errorf("no stats file configured")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read stats file: %w", err)
	}

	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("parse stats file: %w", err)
	}
	if snap.Version != snapshotVersion {
		return fmt.Errorf("stats file version %d, want %d", snap.Version, snapshotVersion)
	}

	cutoff := time.Now().Add(-time.Duration(c.cfg.HistoryMinutes) * c.cfg.BucketInterval).Unix()

	c.mu.Lock()
	defer c.mu.Unlock()

	c.totals = snap.Totals
	c.direct = snap.Direct
	c.proxy = snap.Proxy
	c.dns = DNSStat{
		Queries:    snap.DNS.Queries,
		CacheHits:  snap.DNS.CacheHits,
		Failures:   snap.DNS.Failures,
		Direct:     snap.DNS.Direct,
		DoH:        snap.DNS.DoH,
		ByType:     snap.DNS.ByType,
		ByUpstream: snap.DNS.ByUpstream,
	}
	if c.dns.ByType == nil {
		c.dns.ByType = map[string]int64{}
	}
	if c.dns.ByUpstream == nil {
		c.dns.ByUpstream = map[string]int64{}
	}
	for i := range snap.DNS.TopDomains {
		d := snap.DNS.TopDomains[i]
		if d.Domain == "" {
			continue
		}
		cp := d
		c.dnsNames[d.Domain] = &cp
	}

	maxBuckets := cap(c.buckets)
	c.buckets = c.buckets[:0]
	c.head = 0
	for _, b := range snap.Buckets {
		if b.T <= 0 || b.T < cutoff {
			continue
		}
		if len(c.buckets) >= maxBuckets {
			// Older buckets fall off the front when the file holds more
			// history than the current window.
			c.buckets = c.buckets[1:]
		}
		c.buckets = append(c.buckets, b)
	}

	for i := range snap.Domains {
		d := snap.Domains[i]
		if d.Domain == "" {
			continue
		}
		cp := d
		c.domains[d.Domain] = &cp
	}
	for i := range snap.Nodes {
		n := snap.Nodes[i]
		if n.Name == "" {
			continue
		}
		cp := n
		c.nodes[n.Name] = &cp
	}
	if len(snap.Errors) > c.cfg.ErrorLimit {
		snap.Errors = snap.Errors[len(snap.Errors)-c.cfg.ErrorLimit:]
	}
	c.errs = snap.Errors
	c.peakUp = snap.PeakUp
	c.peakDown = snap.PeakDown
	return nil
}

func allDomainsLocked(m map[string]*DomainStat) []DomainStat {
	out := make([]DomainStat, 0, len(m))
	for _, d := range m {
		out = append(out, *d)
	}
	return out
}

func allNodesLocked(m map[string]*NodeStat) []NodeStat {
	out := make([]NodeStat, 0, len(m))
	for _, n := range m {
		out = append(out, *n)
	}
	return out
}
