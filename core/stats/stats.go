// Package stats collects live and historical traffic statistics for the
// proxy: per-session byte accounting, minute-resolution history, domain,
// node and routing breakdowns, DNS counters, transfer rates and a rolling
// error log. Package api exposes this data over HTTP.
//
// Direction conventions, as seen by the relay (yellowsocks):
//
//	Upload   = client -> target (bytes the client sends out)
//	Download = target -> client (bytes the client receives)
package stats

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/esrrhs/gohome/loggo"
)

func logWarn(format string, args ...interface{}) {
	loggo.Warn("[Stats] "+format, args...)
}

// Defaults for Config.
const (
	DefaultHistoryMinutes = 1440 // 24h at one-minute resolution
	DefaultBucketInterval = time.Minute
	DefaultSaveInterval   = time.Minute
	DefaultDomainLimit    = 3000
	DefaultErrorLimit     = 200

	// rateSlots is the number of one-second slots kept for speed sampling.
	rateSlots = 60
	// maxSeriesPoints bounds how many points Series emits.
	maxSeriesPoints = 2000
	// flushThreshold is how much traffic a session accumulates before its
	// bytes are committed to the domain table. Committing on every 32 KiB
	// relay chunk would take the lock far too often.
	flushThreshold = 1 << 20
)

// Proto is the inbound protocol that produced a session.
type Proto string

const (
	ProtoSOCKS5 Proto = "socks5"
	ProtoHTTP   Proto = "http"
	ProtoUDP    Proto = "udp" // SOCKS5 UDP ASSOCIATE relay
	ProtoDNS    Proto = "dns"
)

// Decision is the routing decision taken for a session.
type Decision string

const (
	DecisionDirect Decision = "direct"
	DecisionProxy  Decision = "proxy"
	DecisionNone   Decision = "" // UDP association, decided per datagram
)

// Counters is a byte/connection/error aggregate.
type Counters struct {
	Upload   int64 `json:"upload_bytes"`
	Download int64 `json:"download_bytes"`
	Conns    int64 `json:"conns"`
	Errors   int64 `json:"errors"`
}

// Total returns upload + download.
func (c Counters) Total() int64 { return c.Upload + c.Download }

func (c *Counters) addUpload(n int64)   { c.Upload += n }
func (c *Counters) addDownload(n int64) { c.Download += n }

// Bucket is one history interval.
type Bucket struct {
	T int64 `json:"t"` // bucket start, unix seconds

	Upload   int64 `json:"upload_bytes"`
	Download int64 `json:"download_bytes"`
	Conns    int64 `json:"conns"`
	Errors   int64 `json:"errors"`

	DirectUpload   int64 `json:"direct_upload_bytes"`
	DirectDownload int64 `json:"direct_download_bytes"`
	ProxyUpload    int64 `json:"proxy_upload_bytes"`
	ProxyDownload  int64 `json:"proxy_download_bytes"`
	DirectConns    int64 `json:"direct_conns"`
	ProxyConns     int64 `json:"proxy_conns"`

	DNSQueries int64 `json:"dns_queries"`
}

// DomainStat aggregates traffic for one destination domain.
type DomainStat struct {
	Domain      string `json:"domain"`
	Upload      int64  `json:"upload_bytes"`
	Download    int64  `json:"download_bytes"`
	Conns       int64  `json:"conns"`
	DirectConns int64  `json:"direct_conns"`
	ProxyConns  int64  `json:"proxy_conns"`
	Errors      int64  `json:"errors"`
	LastSeen    int64  `json:"last_seen"`
}

// Total returns upload + download.
func (d DomainStat) Total() int64 { return d.Upload + d.Download }

// NodeStat aggregates traffic forwarded through one SPP node.
type NodeStat struct {
	Name     string `json:"name"`
	Upload   int64  `json:"upload_bytes"`
	Download int64  `json:"download_bytes"`
	Conns    int64  `json:"conns"`
	LastSeen int64  `json:"last_seen"`
}

// ErrorEntry is one recorded failure.
type ErrorEntry struct {
	T       int64  `json:"t"` // unix seconds
	Time    string `json:"time"`
	Scope   string `json:"scope"` // socks5, http, dns, spp, engine
	Host    string `json:"host"`
	Message string `json:"message"`
}

// SessionView is a live or finished relay as reported by the API.
type SessionView struct {
	ID        int64  `json:"id"`
	Proto     string `json:"proto"`
	Client    string `json:"client"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Decision  string `json:"decision"`
	Node      string `json:"node"`
	Start     int64  `json:"start"` // unix seconds
	AgeMs     int64  `json:"age_ms"`
	Upload    int64  `json:"upload_bytes"`
	Download  int64  `json:"download_bytes"`
	UploadBps int64  `json:"upload_bps"`
	DownBps   int64  `json:"download_bps"`
}

// Rate is a transfer speed sample in bytes per second.
type Rate struct {
	Upload   int64 `json:"upload_bps"`
	Download int64 `json:"download_bps"`
}

// Config configures a Collector. Zero values select the defaults.
type Config struct {
	// HistoryMinutes is how many buckets are kept (default 1440).
	HistoryMinutes int
	// BucketInterval is the resolution of one bucket (default 1 minute).
	BucketInterval time.Duration
	// File persists counters and history across restarts. Empty disables it.
	File string
	// SaveInterval is how often the snapshot is written (default 1 minute).
	// Zero or negative disables background saving.
	SaveInterval time.Duration
	// DomainLimit caps how many domains are tracked (default 3000).
	DomainLimit int
	// ErrorLimit caps the rolling error log (default 200).
	ErrorLimit int
}

// Collector owns every counter. All methods are safe for concurrent use.
type Collector struct {
	cfg Config

	mu       sync.RWMutex
	start    time.Time
	active   map[int64]*Session
	totals   Counters
	direct   Counters
	proxy    Counters
	dns      DNSStat
	dnsNames map[string]*DomainStat
	domains  map[string]*DomainStat
	nodes    map[string]*NodeStat
	errs     []ErrorEntry

	// History is a fixed-size ring of buckets plus the bucket being filled.
	buckets []Bucket
	head    int
	cur     Bucket

	// One-second rate slots, indexed by unix second modulo rateSlots.
	rateSec  int64
	rateUp   [rateSlots]int64
	rateDown [rateSlots]int64
	peakUp   int64
	peakDown int64

	nextID atomic.Int64

	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// DNSStat counts DNS queries handled by the embedded resolver.
type DNSStat struct {
	Queries    int64            `json:"queries"`
	CacheHits  int64            `json:"cache_hits"`
	Failures   int64            `json:"failures"`
	Direct     int64            `json:"direct"`      // answered by the direct DNS server
	DoH        int64            `json:"doh"`         // answered by the remote DoH resolver
	ByType     map[string]int64 `json:"by_type"`     // "A", "AAAA", ...
	ByUpstream map[string]int64 `json:"by_upstream"` // "cache", "direct", "doh", "synthetic"
	TopDomains []DomainStat     `json:"top_domains"`
}

// New builds a collector, restores any persisted snapshot and starts the
// background saver when persistence is enabled.
func New(cfg Config) *Collector {
	if cfg.HistoryMinutes <= 0 {
		cfg.HistoryMinutes = DefaultHistoryMinutes
	}
	if cfg.BucketInterval <= 0 {
		cfg.BucketInterval = DefaultBucketInterval
	}
	if cfg.DomainLimit <= 0 {
		cfg.DomainLimit = DefaultDomainLimit
	}
	if cfg.ErrorLimit <= 0 {
		cfg.ErrorLimit = DefaultErrorLimit
	}

	now := time.Now()
	c := &Collector{
		cfg:      cfg,
		start:    now,
		active:   make(map[int64]*Session),
		dns:      DNSStat{ByType: map[string]int64{}, ByUpstream: map[string]int64{}},
		dnsNames: make(map[string]*DomainStat),
		domains:  make(map[string]*DomainStat),
		nodes:    make(map[string]*NodeStat),
		buckets:  make([]Bucket, 0, cfg.HistoryMinutes),
		stopCh:   make(chan struct{}),
	}
	c.cur.T = alignUnix(now.Unix(), cfg.BucketInterval)
	c.rateSec = now.Unix()

	if cfg.File != "" {
		if err := c.Load(); err != nil {
			// A corrupt snapshot must never stop the proxy from starting.
			logWarn("load stats snapshot %s failed: %v", cfg.File, err)
		}
	}

	if cfg.File != "" && cfg.SaveInterval > 0 {
		c.wg.Add(1)
		go c.saveLoop()
	}
	return c
}

// StartTime is when the collector was created.
func (c *Collector) StartTime() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.start
}

// Close stops the background saver and writes a final snapshot.
func (c *Collector) Close() {
	c.stopOnce.Do(func() { close(c.stopCh) })
	c.wg.Wait()
	if c.cfg.File != "" {
		if err := c.Save(); err != nil {
			logWarn("save stats snapshot %s failed: %v", c.cfg.File, err)
		}
	}
}

func (c *Collector) saveLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.cfg.SaveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			if err := c.Save(); err != nil {
				logWarn("save stats snapshot %s failed: %v", c.cfg.File, err)
			}
		}
	}
}

// Session is one relay (a TCP proxy connection or a UDP association). It is
// created by Begin and must be closed with End, exactly once.
//
// A nil *Session is valid: every method is a no-op on it, so callers can
// wire statistics in unconditionally and stay inert when no collector is
// configured.
type Session struct {
	c        *Collector
	id       int64
	proto    Proto
	client   string
	host     string
	port     int
	decision Decision
	node     string
	start    time.Time

	upload      atomic.Int64
	download    atomic.Int64
	pendingUp   atomic.Int64
	pendingDown atomic.Int64
	done        atomic.Bool
}

// Begin registers a new relay. It returns nil when c is nil.
func (c *Collector) Begin(p Proto, client, host string, port int, d Decision, node string) *Session {
	if c == nil {
		return nil
	}
	s := &Session{
		c:        c,
		id:       c.nextID.Add(1),
		proto:    p,
		client:   client,
		host:     host,
		port:     port,
		decision: d,
		node:     node,
		start:    time.Now(),
	}
	c.mu.Lock()
	c.active[s.id] = s
	c.mu.Unlock()
	return s
}

// AddUpload records n bytes sent by the client towards the target.
func (s *Session) AddUpload(n int64) {
	if s == nil || n <= 0 {
		return
	}
	s.upload.Add(n)
	s.c.addTraffic(n, 0, s.decision)
	if s.pendingUp.Add(n) >= flushThreshold {
		s.flush()
	}
}

// AddDownload records n bytes returned by the target to the client.
func (s *Session) AddDownload(n int64) {
	if s == nil || n <= 0 {
		return
	}
	s.download.Add(n)
	s.c.addTraffic(0, n, s.decision)
	if s.pendingDown.Add(n) >= flushThreshold {
		s.flush()
	}
}

// AddDatagram records one relayed UDP datagram and attributes its bytes to
// host. UDP associations multiplex many destinations, so the per-domain
// counters are updated per datagram while the session itself tracks the
// association totals.
func (s *Session) AddDatagram(host string, d Decision, up, down int64) {
	if s == nil {
		return
	}
	if up > 0 {
		s.upload.Add(up)
		s.pendingUp.Add(up)
	}
	if down > 0 {
		s.download.Add(down)
		s.pendingDown.Add(down)
	}
	if up > 0 || down > 0 {
		s.c.addTraffic(up, down, d)
	}
	if host != "" {
		s.c.addDomain(host, d, up, down, 0, false)
	}
	s.flush()
}

// End closes the session and commits its counters. err, when non-nil, is
// recorded as an error for the session's host. End is idempotent.
func (s *Session) End(err error) {
	if s == nil {
		return
	}
	if !s.done.CompareAndSwap(false, true) {
		return
	}
	s.c.finish(s, err)
}

// Fail closes the session recording a failure. It is End with an error.
func (s *Session) Fail(err error) { s.End(err) }

// Upload returns the bytes sent by the client so far.
func (s *Session) Upload() int64 {
	if s == nil {
		return 0
	}
	return s.upload.Load()
}

// Download returns the bytes returned to the client so far.
func (s *Session) Download() int64 {
	if s == nil {
		return 0
	}
	return s.download.Load()
}

func (s *Session) flush() {
	up := s.pendingUp.Swap(0)
	down := s.pendingDown.Swap(0)
	if up == 0 && down == 0 {
		return
	}
	if s.host != "" {
		s.c.addDomain(s.host, s.decision, up, down, 0, false)
	}
}

func (s *Session) view(now time.Time) SessionView {
	age := now.Sub(s.start)
	up, down := s.upload.Load(), s.download.Load()
	secs := int64(age / time.Second)
	var upBps, downBps int64
	if secs > 0 {
		upBps, downBps = up/secs, down/secs
	}
	return SessionView{
		ID:        s.id,
		Proto:     string(s.proto),
		Client:    s.client,
		Host:      s.host,
		Port:      s.port,
		Decision:  string(s.decision),
		Node:      s.node,
		Start:     s.start.Unix(),
		AgeMs:     age.Milliseconds(),
		Upload:    up,
		Download:  down,
		UploadBps: upBps,
		DownBps:   downBps,
	}
}

// finish removes the session from the active map and commits its counters.
func (c *Collector) finish(s *Session, err error) {
	now := time.Now()
	up := s.upload.Load()
	down := s.download.Load()

	c.mu.Lock()
	delete(c.active, s.id)
	c.totals.Conns++
	c.bumpConnLocked(s.decision, now)

	if s.host != "" {
		// Byte counters were pushed to the domain table in flushThreshold
		// chunks while the relay ran; commit the remainder with the session.
		pu := s.pendingUp.Swap(0)
		pd := s.pendingDown.Swap(0)
		c.addDomainLocked(s.host, s.decision, pu, pd, 1, err != nil)
	}
	if s.decision == DecisionProxy && s.node != "" {
		n := c.nodes[s.node]
		if n == nil {
			n = &NodeStat{Name: s.node}
			c.nodes[s.node] = n
		}
		n.Conns++
		n.Upload += up
		n.Download += down
		n.LastSeen = now.Unix()
	}
	if err != nil {
		c.recordErrorLocked("proxy", s.host, err.Error(), now)
	}
	c.mu.Unlock()
}

// addTraffic updates the live byte counters, the current history bucket and
// the per-second rate slots.
func (c *Collector) addTraffic(up, down int64, d Decision) {
	if up == 0 && down == 0 {
		return
	}
	now := time.Now()
	nowSec := now.Unix()

	c.mu.Lock()
	c.rotateLocked(now)
	c.totals.addUpload(up)
	c.totals.addDownload(down)
	c.cur.Upload += up
	c.cur.Download += down
	switch d {
	case DecisionDirect:
		c.direct.addUpload(up)
		c.direct.addDownload(down)
		c.cur.DirectUpload += up
		c.cur.DirectDownload += down
	case DecisionProxy:
		c.proxy.addUpload(up)
		c.proxy.addDownload(down)
		c.cur.ProxyUpload += up
		c.cur.ProxyDownload += down
	}
	c.addRateLocked(nowSec, up, down)
	c.mu.Unlock()
}

func (c *Collector) bumpConnLocked(d Decision, now time.Time) {
	c.rotateLocked(now)
	c.cur.Conns++
	switch d {
	case DecisionDirect:
		c.direct.Conns++
		c.cur.DirectConns++
	case DecisionProxy:
		c.proxy.Conns++
		c.cur.ProxyConns++
	}
}

func (c *Collector) addRateLocked(nowSec, up, down int64) {
	if c.rateSec != nowSec {
		if nowSec > c.rateSec {
			gap := nowSec - c.rateSec
			if gap >= rateSlots {
				c.rateUp = [rateSlots]int64{}
				c.rateDown = [rateSlots]int64{}
			} else {
				for i := int64(1); i <= gap; i++ {
					idx := int((c.rateSec + i) % rateSlots)
					c.rateUp[idx] = 0
					c.rateDown[idx] = 0
				}
			}
		}
		c.rateSec = nowSec
	}
	idx := int(nowSec % rateSlots)
	c.rateUp[idx] += up
	c.rateDown[idx] += down
	if c.rateUp[idx] > c.peakUp {
		c.peakUp = c.rateUp[idx]
	}
	if c.rateDown[idx] > c.peakDown {
		c.peakDown = c.rateDown[idx]
	}
}

// rateLocked sums the last window seconds, excluding the partially filled
// current second, and returns bytes per second.
func (c *Collector) rateLocked(nowSec int64, window int) Rate {
	if window <= 0 || window >= rateSlots {
		window = rateSlots - 1
	}
	var up, down int64
	for i := 1; i <= window; i++ {
		idx := int((nowSec - int64(i)) % rateSlots)
		if idx < 0 {
			idx += rateSlots
		}
		up += c.rateUp[idx]
		down += c.rateDown[idx]
	}
	w := int64(window)
	return Rate{Upload: up / w, Download: down / w}
}

// rotateLocked closes history buckets that have elapsed and opens the one
// covering now.
func (c *Collector) rotateLocked(now time.Time) {
	interval := int64(c.cfg.BucketInterval / time.Second)
	if interval <= 0 {
		interval = 60
	}
	want := alignUnix(now.Unix(), c.cfg.BucketInterval)
	if c.cur.T == 0 {
		c.cur.T = want
		return
	}
	for c.cur.T < want {
		c.pushBucketLocked(c.cur)
		c.cur = Bucket{T: c.cur.T + interval}
	}
}

func (c *Collector) pushBucketLocked(b Bucket) {
	if len(c.buckets) < cap(c.buckets) {
		c.buckets = append(c.buckets, b)
		return
	}
	c.buckets[c.head] = b
	c.head = (c.head + 1) % len(c.buckets)
}

// orderedBucketsLocked returns the history ring in chronological order. The
// caller must hold the lock; the returned slice is a copy.
func (c *Collector) orderedBucketsLocked() []Bucket {
	out := make([]Bucket, 0, len(c.buckets)+1)
	n := len(c.buckets)
	for i := 0; i < n; i++ {
		b := c.buckets[(c.head+i)%n]
		if b.T > 0 {
			out = append(out, b)
		}
	}
	return out
}

func alignUnix(t int64, interval time.Duration) int64 {
	secs := int64(interval / time.Second)
	if secs <= 0 {
		secs = 60
	}
	return t / secs * secs
}

// RecordError adds an entry to the rolling error log. Scope is the
// subsystem that failed (socks5, http, dns, spp, engine).
func (c *Collector) RecordError(scope, host, format string, args ...interface{}) {
	if c == nil {
		return
	}
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	now := time.Now()
	c.mu.Lock()
	c.recordErrorLocked(scope, host, msg, now)
	c.mu.Unlock()
}

func (c *Collector) recordErrorLocked(scope, host, msg string, now time.Time) {
	c.totals.Errors++
	c.rotateLocked(now)
	c.cur.Errors++
	c.errs = append(c.errs, ErrorEntry{
		T:       now.Unix(),
		Time:    now.Format(time.RFC3339),
		Scope:   scope,
		Host:    host,
		Message: msg,
	})
	if len(c.errs) > c.cfg.ErrorLimit {
		c.errs = append(c.errs[:0], c.errs[len(c.errs)-c.cfg.ErrorLimit:]...)
	}
}

// RecordDNS records one resolved query. upstream is one of cache, direct,
// doh or synthetic; err is the resolution failure, if any.
func (c *Collector) RecordDNS(domain, qtype, upstream string, err error) {
	if c == nil {
		return
	}
	now := time.Now()
	c.mu.Lock()
	c.rotateLocked(now)
	c.dns.Queries++
	c.cur.DNSQueries++
	if c.dns.ByType == nil {
		c.dns.ByType = map[string]int64{}
	}
	if c.dns.ByUpstream == nil {
		c.dns.ByUpstream = map[string]int64{}
	}
	if qtype != "" {
		c.dns.ByType[qtype]++
	}
	c.dns.ByUpstream[upstream]++
	switch upstream {
	case "cache":
		c.dns.CacheHits++
	case "direct":
		c.dns.Direct++
	case "doh":
		c.dns.DoH++
	}
	if err != nil {
		c.dns.Failures++
		c.recordErrorLocked("dns", domain, err.Error(), now)
	}
	if domain != "" {
		d := c.dnsNames[domain]
		if d == nil {
			d = &DomainStat{Domain: domain}
			c.dnsNames[domain] = d
		}
		d.Conns++
		d.LastSeen = now.Unix()
		if err != nil {
			d.Errors++
		}
		if len(c.dnsNames) > c.cfg.DomainLimit {
			c.pruneLocked(c.dnsNames)
		}
	}
	c.mu.Unlock()
}

// addDomain merges traffic into the domain table.
func (c *Collector) addDomain(host string, d Decision, up, down, conns int64, isErr bool) {
	c.mu.Lock()
	c.addDomainLocked(host, d, up, down, conns, isErr)
	c.mu.Unlock()
}

func (c *Collector) addDomainLocked(host string, dec Decision, up, down, conns int64, isErr bool) {
	now := time.Now()
	e := c.domains[host]
	if e == nil {
		e = &DomainStat{Domain: host}
		c.domains[host] = e
	}
	e.Upload += up
	e.Download += down
	e.Conns += conns
	switch dec {
	case DecisionDirect:
		e.DirectConns += conns
	case DecisionProxy:
		e.ProxyConns += conns
	}
	if isErr {
		e.Errors++
	}
	e.LastSeen = now.Unix()
	if len(c.domains) > c.cfg.DomainLimit {
		c.pruneLocked(c.domains)
	}
}

// pruneLocked drops the least busy domains when the table is over its cap.
func (c *Collector) pruneLocked(m map[string]*DomainStat) {
	keep := c.cfg.DomainLimit * 3 / 4
	if keep <= 0 {
		keep = 1
	}
	all := make([]*DomainStat, 0, len(m))
	for _, d := range m {
		all = append(all, d)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Total() > all[j].Total() })
	for _, d := range all[keep:] {
		delete(m, d.Domain)
	}
}

// Overview is the live summary returned by the API.
type Overview struct {
	Now           int64        `json:"now"`
	StartTime     int64        `json:"start_time"`
	UptimeSeconds int64        `json:"uptime_seconds"`
	ActiveConns   int          `json:"active_conns"`
	Totals        Counters     `json:"totals"`
	Direct        Counters     `json:"direct"`
	Proxy         Counters     `json:"proxy"`
	Rate          Rate         `json:"rate"`    // last 5 seconds
	Rate1m        Rate         `json:"rate_1m"` // last 60 seconds
	Peak          Rate         `json:"peak"`    // fastest second seen
	DNS           DNSStat      `json:"dns"`
	TopDomains    []DomainStat `json:"top_domains"`
}

// ActiveOptions filters the live connection list.
type ActiveOptions struct {
	Limit    int
	Proto    string
	Decision string
	Host     string // substring match
}

// Overview returns the current snapshot: totals, split, speeds and the
// busiest domains.
func (c *Collector) Overview() Overview {
	now := time.Now()
	nowSec := now.Unix()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.rotateLocked(now)

	o := Overview{
		Now:           nowSec,
		StartTime:     c.start.Unix(),
		UptimeSeconds: int64(now.Sub(c.start) / time.Second),
		ActiveConns:   len(c.active),
		Totals:        c.totals,
		Direct:        c.direct,
		Proxy:         c.proxy,
		Rate:          c.rateLocked(nowSec, 5),
		Rate1m:        c.rateLocked(nowSec, 60),
		Peak:          Rate{Upload: c.peakUp, Download: c.peakDown},
		DNS:           c.dnsSnapshotLocked(10),
		TopDomains:    c.topDomainsLocked(10, "bytes"),
	}
	return o
}

// ActiveSessions returns the live sessions, newest first.
func (c *Collector) ActiveSessions(opt ActiveOptions) []SessionView {
	if opt.Limit <= 0 {
		opt.Limit = 100
	}
	now := time.Now()

	c.mu.RLock()
	all := make([]*Session, 0, len(c.active))
	for _, s := range c.active {
		if opt.Proto != "" && string(s.proto) != opt.Proto {
			continue
		}
		if opt.Decision != "" && string(s.decision) != opt.Decision {
			continue
		}
		if opt.Host != "" && !containsFold(s.host, opt.Host) {
			continue
		}
		all = append(all, s)
	}
	c.mu.RUnlock()

	sort.Slice(all, func(i, j int) bool { return all[i].id > all[j].id })
	if len(all) > opt.Limit {
		all = all[:opt.Limit]
	}
	out := make([]SessionView, 0, len(all))
	for _, s := range all {
		out = append(out, s.view(now))
	}
	return out
}

// Config returns the collector's configuration as resolved by New.
func (c *Collector) Config() Config { return c.cfg }

// TopDomainsByDecision returns the busiest domains that were routed through
// one path (direct or proxy).
func (c *Collector) TopDomainsByDecision(limit int, d Decision, order string) []DomainStat {
	c.mu.RLock()
	out := make([]DomainStat, 0, len(c.domains))
	for _, e := range c.domains {
		if d == DecisionDirect && e.DirectConns == 0 {
			continue
		}
		if d == DecisionProxy && e.ProxyConns == 0 {
			continue
		}
		out = append(out, *e)
	}
	c.mu.RUnlock()

	if order == "conns" {
		sort.Slice(out, func(i, j int) bool { return out[i].Conns > out[j].Conns })
	} else {
		sort.Slice(out, func(i, j int) bool { return out[i].Total() > out[j].Total() })
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Domains returns the domain table, ordered by bytes or connections.
func (c *Collector) Domains(limit int, order string) []DomainStat {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.topDomainsLocked(limit, order)
}

func (c *Collector) topDomainsLocked(limit int, order string) []DomainStat {
	if limit <= 0 {
		limit = 50
	}
	out := make([]DomainStat, 0, len(c.domains))
	for _, d := range c.domains {
		out = append(out, *d)
	}
	if order == "conns" {
		sort.Slice(out, func(i, j int) bool {
			if out[i].Conns == out[j].Conns {
				return out[i].Total() > out[j].Total()
			}
			return out[i].Conns > out[j].Conns
		})
	} else {
		sort.Slice(out, func(i, j int) bool {
			if out[i].Total() == out[j].Total() {
				return out[i].Conns > out[j].Conns
			}
			return out[i].Total() > out[j].Total()
		})
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Nodes returns per-SPP-node traffic. Names are the node names used when the
// traffic was forwarded, so a node removed from the config keeps its totals.
func (c *Collector) Nodes() []NodeStat {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]NodeStat, 0, len(c.nodes))
	for _, n := range c.nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Upload+out[i].Download > out[j].Upload+out[j].Download })
	return out
}

// Errors returns the most recent error entries, newest last.
func (c *Collector) Errors(limit int) []ErrorEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if limit <= 0 || limit > len(c.errs) {
		limit = len(c.errs)
	}
	out := make([]ErrorEntry, limit)
	copy(out, c.errs[len(c.errs)-limit:])
	return out
}

// DNSStats returns DNS counters, optionally with the top queried domains.
func (c *Collector) DNSStats(topN int) DNSStat {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dnsSnapshotLocked(topN)
}

func (c *Collector) dnsSnapshotLocked(topN int) DNSStat {
	d := DNSStat{
		Queries:    c.dns.Queries,
		CacheHits:  c.dns.CacheHits,
		Failures:   c.dns.Failures,
		Direct:     c.dns.Direct,
		DoH:        c.dns.DoH,
		ByType:     copyMap(c.dns.ByType),
		ByUpstream: copyMap(c.dns.ByUpstream),
	}
	if topN > 0 {
		top := c.topFromLocked(c.dnsNames, topN)
		d.TopDomains = top
	}
	return d
}

func (c *Collector) topFromLocked(m map[string]*DomainStat, n int) []DomainStat {
	out := make([]DomainStat, 0, len(m))
	for _, d := range m {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Conns == out[j].Conns {
			return out[i].Total() > out[j].Total()
		}
		return out[i].Conns > out[j].Conns
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// SeriesPoint is one aggregated history interval.
type SeriesPoint struct {
	T              int64  `json:"t"`
	Time           string `json:"time"`
	Upload         int64  `json:"upload_bytes"`
	Download       int64  `json:"download_bytes"`
	Conns          int64  `json:"conns"`
	Errors         int64  `json:"errors"`
	DirectUpload   int64  `json:"direct_upload_bytes"`
	DirectDownload int64  `json:"direct_download_bytes"`
	ProxyUpload    int64  `json:"proxy_upload_bytes"`
	ProxyDownload  int64  `json:"proxy_download_bytes"`
	DNSQueries     int64  `json:"dns_queries"`
}

// Series returns history aggregated into step-sized intervals covering
// [since, now]. Missing intervals are filled with zeroes so callers can plot
// the result directly.
func (c *Collector) Series(since time.Time, step time.Duration) []SeriesPoint {
	if step <= 0 {
		step = c.cfg.BucketInterval
	}
	stepSec := int64(step / time.Second)
	if stepSec <= 0 {
		stepSec = 60
	}
	now := time.Now()

	c.mu.Lock()
	c.rotateLocked(now)
	// The bucket being filled is part of the series: traffic from the last
	// minute must show up without waiting for it to close.
	buckets := c.orderedBucketsLocked()
	if c.cur.T > 0 {
		buckets = append(buckets, c.cur)
	}
	c.mu.Unlock()

	start := alignUnix(since.Unix(), step)
	end := alignUnix(now.Unix(), step)
	if end < start {
		return nil
	}
	// Cap the emitted points: a huge range with a small step must not
	// allocate a slice sized by the request alone. The window slides
	// forward so the newest data is always included.
	if (end-start)/stepSec+1 > maxSeriesPoints {
		start = end - (maxSeriesPoints-1)*stepSec
	}

	agg := make(map[int64]*SeriesPoint)
	for i := range buckets {
		b := buckets[i]
		k := alignUnix(b.T, step)
		p := agg[k]
		if p == nil {
			p = &SeriesPoint{T: k}
			agg[k] = p
		}
		p.Upload += b.Upload
		p.Download += b.Download
		p.Conns += b.Conns
		p.Errors += b.Errors
		p.DirectUpload += b.DirectUpload
		p.DirectDownload += b.DirectDownload
		p.ProxyUpload += b.ProxyUpload
		p.ProxyDownload += b.ProxyDownload
		p.DNSQueries += b.DNSQueries
	}

	out := make([]SeriesPoint, 0, (end-start)/stepSec+1)
	for t := start; t <= end; t += stepSec {
		p := SeriesPoint{T: t, Time: time.Unix(t, 0).Format(time.RFC3339)}
		if a, ok := agg[t]; ok {
			p.Upload = a.Upload
			p.Download = a.Download
			p.Conns = a.Conns
			p.Errors = a.Errors
			p.DirectUpload = a.DirectUpload
			p.DirectDownload = a.DirectDownload
			p.ProxyUpload = a.ProxyUpload
			p.ProxyDownload = a.ProxyDownload
			p.DNSQueries = a.DNSQueries
		}
		out = append(out, p)
	}
	return out
}

// HistoryWindow returns the span covered by the in-memory history.
func (c *Collector) HistoryWindow() (oldest, newest time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	b := c.orderedBucketsLocked()
	if c.cur.T > 0 {
		b = append(b, c.cur)
	}
	if len(b) == 0 {
		return c.start, time.Now()
	}
	return time.Unix(b[0].T, 0), time.Unix(b[len(b)-1].T, 0)
}

// Reset clears every counter, history and log. Active sessions are kept so a
// reset cannot strand them.
func (c *Collector) Reset() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.totals = Counters{}
	c.direct = Counters{}
	c.proxy = Counters{}
	c.dns = DNSStat{ByType: map[string]int64{}, ByUpstream: map[string]int64{}}
	c.dnsNames = make(map[string]*DomainStat)
	c.domains = make(map[string]*DomainStat)
	c.nodes = make(map[string]*NodeStat)
	c.errs = nil
	c.buckets = c.buckets[:0]
	c.head = 0
	c.cur = Bucket{T: alignUnix(now.Unix(), c.cfg.BucketInterval)}
	c.rateUp = [rateSlots]int64{}
	c.rateDown = [rateSlots]int64{}
	c.peakUp = 0
	c.peakDown = 0
	c.start = now
}

func copyMap(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func containsFold(s, sub string) bool {
	if sub == "" {
		return true
	}
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
