package stats

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// snapshotVersion is the version written by this build. Version 1 predates
// the lifetime counters; version 2 adds them (and distinguishes a deliberate
// lifetime reset from a legacy file, which is seeded from the totals).
// Files up to the current version are readable; anything newer is ignored:
// statistics are never important enough to stop the proxy from starting.
const snapshotVersion = 2

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
//
// Totals, Direct, Proxy and the peak rates are written but never read back:
// they are this-launch counters, kept in the file as a record of the last
// session and as the source the version-1 seeding path needs. Everything that
// has to outlive a restart lives in the Lifetime* fields or in Buckets.
type snapshot struct {
	Version        int          `json:"version"`
	SavedAt        time.Time    `json:"saved_at"`
	Totals         Counters     `json:"totals"`
	Direct         Counters     `json:"direct"`
	Proxy          Counters     `json:"proxy"`
	Lifetime       Counters     `json:"lifetime,omitempty"`
	LifetimeDirect Counters     `json:"lifetime_direct,omitempty"`
	LifetimeProxy  Counters     `json:"lifetime_proxy,omitempty"`
	DNS            persistedDNS `json:"dns"`
	Buckets        []Bucket     `json:"buckets"`
	Domains        []DomainStat `json:"domains"`
	Nodes          []NodeStat   `json:"nodes"`
	Errors         []ErrorEntry `json:"errors"`
	PeakUp         int64        `json:"peak_upload_bps"`
	PeakDown       int64        `json:"peak_download_bps"`
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
		Version:        snapshotVersion,
		SavedAt:        time.Now(),
		Totals:         c.totals,
		Direct:         c.direct,
		Proxy:          c.proxy,
		Lifetime:       c.lifetime,
		LifetimeDirect: c.lifetimeDirect,
		LifetimeProxy:  c.lifetimeProxy,
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
	if snap.Version < 1 || snap.Version > snapshotVersion {
		return fmt.Errorf("stats file version %d not supported (1..%d)", snap.Version, snapshotVersion)
	}

	cutoff := time.Now().Add(-time.Duration(c.cfg.HistoryMinutes) * c.cfg.BucketInterval).Unix()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Totals, the direct/proxy split and the peak rates are session counters:
	// they answer "how much has this launch moved" and start at zero again,
	// which is what the home page's counters claim to be. All-time numbers
	// live in the lifetime counters below, and the chart reads the buckets.
	// (The snapshot still writes the totals: a version-1 file has nothing
	// else to seed the lifetime counters from.)
	c.lifetime = snap.Lifetime
	c.lifetimeDirect = snap.LifetimeDirect
	c.lifetimeProxy = snap.LifetimeProxy
	// Snapshots written before the lifetime counters existed (version 1)
	// carry no lifetime fields. Seed them from the accumulated totals so the
	// all-time numbers do not silently restart from zero. Version 2 files
	// always carry the fields, including after a deliberate LifetimeReset.
	if snap.Version < 2 && c.lifetime.Total() == 0 && snap.Totals.Total() > 0 {
		c.lifetime = snap.Totals
		c.lifetimeDirect = snap.Direct
		c.lifetimeProxy = snap.Proxy
	}
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
		// Snapshots written before the byte split existed carry totals with
		// no split (or a split that only covers bytes written by a newer
		// build after it loaded the same file); recover the unknown remainder
		// from the connection counts, which have been recorded all along.
		// Entries with no connection to infer from (UDP-only datagram rows)
		// keep what they have: they stay in the table but on neither side,
		// rather than being guessed.
		if total := d.Upload + d.Download; total > 0 {
			if unknown := total - (d.DirectBytes + d.ProxyBytes); unknown > 0 {
				if d.DirectConns > 0 || d.ProxyConns > 0 {
					share := float64(d.DirectConns) / float64(d.DirectConns+d.ProxyConns)
					add := int64(float64(unknown)*share + 0.5)
					d.DirectBytes += add
					d.ProxyBytes += unknown - add
				}
			}
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
	// Peaks stay with the session too: a "peak rate" that outlives the totals
	// it was measured against reads as a bug ("2 MB/s peak, 340 MB moved?").
	return nil
}

func allDomainsLocked(m map[string]*DomainStat) []DomainStat {
	out := make([]DomainStat, 0, len(m))
	for _, d := range m {
		out = append(out, *d)
	}
	return out
}

// LifetimeTotals reads the permanent counters straight from a snapshot file,
// without building a collector. It is meant for frontends that show the
// all-time totals while the engine is stopped. Missing or unreadable files
// yield zero counters.
func LifetimeTotals(path string) (all, direct, proxy Counters) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Counters{}, Counters{}, Counters{}
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return Counters{}, Counters{}, Counters{}
	}
	if snap.Version < 2 && snap.Lifetime.Total() == 0 && snap.Totals.Total() > 0 {
		// Version 1 snapshot without lifetime fields: same seeding as Load.
		return snap.Totals, snap.Direct, snap.Proxy
	}
	return snap.Lifetime, snap.LifetimeDirect, snap.LifetimeProxy
}

// DomainTable reads the persisted per-destination table straight from a
// snapshot file, ordered the way it was written (unsorted). Companion to
// LifetimeTotals: the destination breakdown keeps its numbers while the
// engine is stopped. Missing or unreadable files yield nil.
func DomainTable(path string) []DomainStat {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil
	}
	return snap.Domains
}

func allNodesLocked(m map[string]*NodeStat) []NodeStat {
	out := make([]NodeStat, 0, len(m))
	for _, n := range m {
		out = append(out, *n)
	}
	return out
}
