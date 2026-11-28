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
	"net"
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

// LifetimeStats is the permanent traffic record: all bytes ever relayed plus
// the domestic (direct) and foreign (proxy) shares. Unlike the session
// counters these survive restarts and are never cleared by Reset.
type LifetimeStats struct {
	Totals Counters `json:"totals"`
	Direct Counters `json:"direct"`
	Proxy  Counters `json:"proxy"`
	// Peak rate over the whole lifetime (fastest single second, up/down).
	PeakUp   int64 `json:"peak_up"`
	PeakDown int64 `json:"peak_down"`
	// MaxConns is the highest concurrent-connection count ever seen; ActiveSec
	// is the accumulated on-time the lifetime averages are divided by.
	MaxConns  int64 `json:"max_conns"`
	ActiveSec int64 `json:"active_sec"`
}

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
	// DirectBytes and ProxyBytes are the same traffic split by routing
	// decision. A domain reachable both ways (a CDN routed direct here,
	// proxy there) keeps one entry and both sides of the split; UDP
	// datagrams carry no single decision and land in neither.
	DirectBytes int64 `json:"direct_bytes"`
	ProxyBytes  int64 `json:"proxy_bytes"`
	Errors      int64 `json:"errors"`
	LastSeen    int64 `json:"last_seen"`
}

// Total returns upload + download.
func (d DomainStat) Total() int64 { return d.Upload + d.Download }

// ProcessStat aggregates traffic for one OS process (the connections
// page's process attribution). UDP datagrams carry no process and land in
// neither byte split.
type ProcessStat struct {
	Name string `json:"name"`
	// Icon is the exe's shell icon as a PNG data URL, captured with the
	// attribution. Empty on platforms without icon extraction.
	Icon        string `json:"icon,omitempty"`
	Upload      int64  `json:"upload_bytes"`
	Download    int64  `json:"download_bytes"`
	Conns       int64  `json:"conns"`
	DirectBytes int64  `json:"direct_bytes"`
	ProxyBytes  int64  `json:"proxy_bytes"`
	LastSeen    int64  `json:"last_seen"`
	// UploadBps/DownloadBps are the speeds measured between the last two
	// Processes samples. The sampling window is the caller's poll interval
	// (the application tab polls once a second), so they answer "who is
	// using the bandwidth right now". The persistence snapshot carries them
	// too, but the first sample after a load only re-establishes the
	// baseline, so a number from a previous run is never reported.
	UploadBps   int64 `json:"upload_bps"`
	DownloadBps int64 `json:"download_bps"`
	// The same live speeds split by routing decision, over the same window:
	// the apps table shows domestic and foreign rates next to the totals.
	DirectUploadBps   int64 `json:"direct_upload_bps"`
	DirectDownloadBps int64 `json:"direct_download_bps"`
	ProxyUploadBps    int64 `json:"proxy_upload_bps"`
	ProxyDownloadBps  int64 `json:"proxy_download_bps"`

	rateAt   time.Time
	rateUp   int64
	rateDown int64
	// Per-decision byte counters feeding the split rates. Private: they only
	// exist to diff between samples, exactly like rateUp/rateDown.
	directUp, directDown int64
	proxyUp, proxyDown   int64
	rateDirectUp         int64
	rateDirectDown       int64
	rateProxyUp          int64
	rateProxyDown        int64
}

// sampleRate folds the bytes accrued since the previous sample into speeds.
// A zero rateAt (a new entry, or one restored from disk) only establishes
// the baseline: there is no trustworthy interval to divide by yet.
func (p *ProcessStat) sampleRate(now time.Time) {
	if p.rateAt.IsZero() {
		p.rateAt = now
		p.rateUp, p.rateDown = p.Upload, p.Download
		p.rateDirectUp, p.rateDirectDown = p.directUp, p.directDown
		p.rateProxyUp, p.rateProxyDown = p.proxyUp, p.proxyDown
		p.UploadBps, p.DownloadBps = 0, 0
		p.DirectUploadBps, p.DirectDownloadBps = 0, 0
		p.ProxyUploadBps, p.ProxyDownloadBps = 0, 0
		return
	}
	dt := now.Sub(p.rateAt)
	if dt < minRateSample {
		return
	}
	ms := dt.Milliseconds()
	p.UploadBps = (p.Upload - p.rateUp) * 1000 / ms
	p.DownloadBps = (p.Download - p.rateDown) * 1000 / ms
	p.DirectUploadBps = (p.directUp - p.rateDirectUp) * 1000 / ms
	p.DirectDownloadBps = (p.directDown - p.rateDirectDown) * 1000 / ms
	p.ProxyUploadBps = (p.proxyUp - p.rateProxyUp) * 1000 / ms
	p.ProxyDownloadBps = (p.proxyDown - p.rateProxyDown) * 1000 / ms
	p.rateAt, p.rateUp, p.rateDown = now, p.Upload, p.Download
	p.rateDirectUp, p.rateDirectDown = p.directUp, p.directDown
	p.rateProxyUp, p.rateProxyDown = p.proxyUp, p.proxyDown
}

// Total returns upload + download.
func (p ProcessStat) Total() int64 { return p.Upload + p.Download }

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
	ID     int64  `json:"id"`
	Proto  string `json:"proto"`
	Client string `json:"client"`
	Host   string `json:"host"`
	// Domain is the name recovered from the connection itself (TLS SNI or the
	// HTTP Host header) when the client dialled an IP literal. Host stays the
	// address that was actually used, because that is what the router and the
	// byte counters saw; Domain only answers "where is this going" for a human.
	Domain   string `json:"domain,omitempty"`
	Port     int    `json:"port"`
	Decision string `json:"decision"`
	Node     string `json:"node"`
	// Process identifies the OS process behind the session (icon as a data
	// URL). Empty when the platform has no resolver or the owner could not
	// be determined.
	ProcessName string `json:"process_name,omitempty"`
	ProcessIcon string `json:"process_icon,omitempty"`
	Start       int64  `json:"start"` // unix seconds
	AgeMs       int64  `json:"age_ms"`
	Upload      int64  `json:"upload_bytes"`
	Download    int64  `json:"download_bytes"`
	UploadBps   int64  `json:"upload_bps"`
	DownBps     int64  `json:"download_bps"`
}

// Rate is a transfer speed sample in bytes per second.
//
// The split fields carry the same speed divided by routing decision: Direct is
// the domestic share, Proxy the foreign one. Every byte is attributed to
// exactly one decision, so the pairs add up to the totals (minus rounding).
// They exist because "how fast" and "how much of it is going abroad" are two
// different questions, and the second one is why a 4 MB/s line can still feel
// slow.
type Rate struct {
	Upload   int64 `json:"upload_bps"`
	Download int64 `json:"download_bps"`

	DirectUpload   int64 `json:"direct_upload_bps"`
	DirectDownload int64 `json:"direct_download_bps"`
	ProxyUpload    int64 `json:"proxy_upload_bps"`
	ProxyDownload  int64 `json:"proxy_download_bps"`
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

	mu     sync.RWMutex
	start  time.Time
	active map[int64]*Session
	totals Counters
	direct Counters
	proxy  Counters
	// Lifetime counters accumulate forever: they survive restarts via the
	// snapshot and are never cleared by Reset. Direct is the domestic
	// (China) share, Proxy the foreign share, as decided by the router.
	lifetime       Counters
	lifetimeDirect Counters
	lifetimeProxy  Counters
	dns            DNSStat
	dnsNames       map[string]*DomainStat
	domains        map[string]*DomainStat
	processes      map[string]*ProcessStat
	nodes          map[string]*NodeStat
	errs           []ErrorEntry

	// History is a fixed-size ring of buckets plus the bucket being filled.
	buckets []Bucket
	head    int
	cur     Bucket

	// One-second rate slots, indexed by unix second modulo rateSlots. The
	// total slots stay the single source for "speed"; the per-decision slots
	// are the same samples split, so a reader never has to add them up.
	rateSec        int64
	rateUp         [rateSlots]int64
	rateDown       [rateSlots]int64
	rateUpDirect   [rateSlots]int64
	rateDownDirect [rateSlots]int64
	rateUpProxy    [rateSlots]int64
	rateDownProxy  [rateSlots]int64
	peakUp         int64
	peakDown       int64
	// Lifetime counterparts of the launch peaks: they survive restarts via
	// the snapshot and only ratchet upward.
	peakAllUp   int64
	peakAllDown int64
	// maxConnsAll is the highest concurrent-connection count ever seen;
	// activeSecAll accumulates the collector's on-time for lifetime averages.
	maxConnsAll    int64
	activeSecAll   int64
	lastActiveTick time.Time

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
		cfg:       cfg,
		start:     now,
		active:    make(map[int64]*Session),
		dns:       DNSStat{ByType: map[string]int64{}, ByUpstream: map[string]int64{}},
		dnsNames:  make(map[string]*DomainStat),
		domains:   make(map[string]*DomainStat),
		processes: make(map[string]*ProcessStat),
		nodes:     make(map[string]*NodeStat),
		buckets:   make([]Bucket, 0, cfg.HistoryMinutes),
		stopCh:    make(chan struct{}),
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
			// The collector being alive IS the on-time: accumulate it for the
			// lifetime averages, with the save interval as the tick length.
			c.mu.Lock()
			c.activeSecAll += int64(c.cfg.SaveInterval / time.Second)
			c.mu.Unlock()
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
	node     atomic.Pointer[string]
	// domain is filled in later, from the traffic itself, when host is an IP
	// literal. It is display-only: host keeps naming the destination for the
	// byte and domain counters, so a sniffed name cannot silently re-attribute
	// bytes that were already counted under the address.
	domain atomic.Pointer[string]
	// udpPeer/udpDecision are the first named datagram's destination and
	// decision, displayed instead of the blank host a UDP association starts
	// with. Atomics, because datagrams relay on several goroutines.
	udpPeer     atomic.Pointer[string]
	udpDecision atomic.Pointer[Decision]
	// process/icon identify the OS process behind the session, attributed
	// once at accept time by an optional resolver. Icon is a data URL.
	process atomic.Pointer[string]
	icon    atomic.Pointer[string]
	start   time.Time

	upload      atomic.Int64
	download    atomic.Int64
	pendingUp   atomic.Int64
	pendingDown atomic.Int64
	done        atomic.Bool

	// Instantaneous rate sampling: each view() compares the byte counters
	// against the previous sample, so the reported speed reflects the recent
	// interval rather than the average over the connection's lifetime.
	rateMu     sync.Mutex
	rateAt     time.Time
	rateUp     int64
	rateDown   int64
	curUpBps   int64
	curDownBps int64
}

// minRateSample is the shortest interval over which a new rate is computed;
// concurrent or rapid API calls inside it reuse the previous sample.
const minRateSample = 500 * time.Millisecond

// rates returns the current upload/download speed in bytes per second.
func (s *Session) rates(now time.Time, up, down int64) (int64, int64) {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	if s.rateAt.IsZero() {
		s.rateAt = s.start
	}
	dt := now.Sub(s.rateAt)
	if dt >= minRateSample {
		ms := dt.Milliseconds()
		s.curUpBps = (up - s.rateUp) * 1000 / ms
		s.curDownBps = (down - s.rateDown) * 1000 / ms
		s.rateAt, s.rateUp, s.rateDown = now, up, down
	}
	return s.curUpBps, s.curDownBps
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
		start:    time.Now(),
	}
	if node != "" {
		s.node.Store(&node)
	}
	c.mu.Lock()
	c.active[s.id] = s
	if n := int64(len(c.active)); n > c.maxConnsAll {
		c.maxConnsAll = n
	}
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
//
// The FIRST named datagram also becomes the association's displayed peer
// (host:port) and its decision: in practice a TUN-mode association is one
// application socket talking to one destination, and a row that says where
// that is beats a blank. Later destinations still count their bytes against
// their own domains; the display simply keeps the peer the association was
// opened for.
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
	if host != "" {
		if s.udpPeer.Load() == nil {
			h := host
			s.udpPeer.Store(&h)
			s.udpDecision.Store(&d)
		}
	}
	s.flush()
}

// SetProcess records the OS process behind the session (name plus an icon
// data URL), attributed once by an optional resolver at accept time. The
// first answer wins: the resolver itself caches, so a later lookup of the
// same socket would only repeat it.
func (s *Session) SetProcess(name, icon string) {
	if s == nil || name == "" {
		return
	}
	if s.process.CompareAndSwap(nil, &name) && icon != "" {
		s.icon.Store(&icon)
	}
}

// Process returns the attributed process name, or "" when unknown.
func (s *Session) Process() string {
	if s == nil {
		return ""
	}
	p := s.process.Load()
	if p == nil {
		return ""
	}
	return *p
}

// ProcessIcon returns the process icon as a data URL, or "" when unknown.
func (s *Session) ProcessIcon() string {
	if s == nil {
		return ""
	}
	p := s.icon.Load()
	if p == nil {
		return ""
	}
	return *p
}

// SetNode sets or updates the SPP upstream node associated with this session.
func (s *Session) SetNode(name string) {
	if s == nil || name == "" {
		return
	}
	s.node.Store(&name)
}

// SetDomain records the name a client asked for, recovered from the relayed
// traffic (TLS SNI, HTTP Host). It only applies to a session that dialled an
// IP literal: a request that named its destination is authoritative and must
// not be overwritten, and a UDP association has no single destination at all.
func (s *Session) SetDomain(name string) {
	if s == nil || name == "" || s.done.Load() {
		return
	}
	if len(name) > 255 || net.ParseIP(s.host) == nil {
		return
	}
	s.domain.Store(&name)
}

// Domain returns the name learned from the traffic, or "" when the session
// still only knows an address.
func (s *Session) Domain() string {
	if s == nil {
		return ""
	}
	ptr := s.domain.Load()
	if ptr == nil {
		return ""
	}
	return *ptr
}

// WantsDomain reports whether this session could use a sniffed name: it was
// opened against an IP literal and nothing has been learned yet. The relay
// asks before it starts copying bytes, so a domain request costs nothing.
func (s *Session) WantsDomain() bool {
	if s == nil || s.host == "" || s.Domain() != "" {
		return false
	}
	return net.ParseIP(s.host) != nil
}

// Node returns the name of the SPP node associated with this session.
func (s *Session) Node() string {
	if s == nil {
		return ""
	}
	ptr := s.node.Load()
	if ptr == nil {
		return ""
	}
	return *ptr
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

// tableKey is the domain-table key this session's bytes belong under. A
// session that dialled an IP literal starts under the address itself; once
// the sniffed name lands, the remaining traffic is committed under it, so
// the destination breakdown names what the connection was actually for
// instead of a CDN address. The few bytes flushed before the name arrived
// stay on the address entry.
func (s *Session) tableKey() string {
	if d := s.Domain(); d != "" {
		return d
	}
	return s.host
}

func (s *Session) flush() {
	up := s.pendingUp.Swap(0)
	down := s.pendingDown.Swap(0)
	if up == 0 && down == 0 {
		return
	}
	if s.host != "" {
		s.c.addDomain(s.tableKey(), s.decision, up, down, 0, false)
	}
	if proc := s.Process(); proc != "" {
		s.c.addProcess(proc, s.ProcessIcon(), s.decision, up, down)
	}
}

func (s *Session) view(now time.Time) SessionView {
	age := now.Sub(s.start)
	up, down := s.upload.Load(), s.download.Load()
	upBps, downBps := s.rates(now, up, down)
	// A UDP association displays the peer its first named datagram revealed;
	// TCP sessions have the destination from the request itself.
	viewHost, viewDecision := s.host, s.decision
	if p := s.udpPeer.Load(); p != nil {
		viewHost = *p
	}
	if d := s.udpDecision.Load(); d != nil {
		viewDecision = *d
	}
	return SessionView{
		ID:          s.id,
		Proto:       string(s.proto),
		Client:      s.client,
		Host:        viewHost,
		Domain:      s.Domain(),
		Port:        s.port,
		Decision:    string(viewDecision),
		Node:        s.Node(),
		ProcessName: s.Process(),
		ProcessIcon: s.ProcessIcon(),
		Start:       s.start.Unix(),
		AgeMs:       age.Milliseconds(),
		Upload:      up,
		Download:    down,
		UploadBps:   upBps,
		DownBps:     downBps,
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
		// The key follows the sniffed name when one landed, so the entry
		// this session ends up under answers "where was this going".
		pu := s.pendingUp.Swap(0)
		pd := s.pendingDown.Swap(0)
		c.addDomainLocked(s.tableKey(), s.decision, pu, pd, 1, err != nil)
		if proc := s.Process(); proc != "" {
			c.addProcessLocked(proc, s.ProcessIcon(), s.decision, pu, pd, 1)
		}
	}
	nodeName := s.Node()
	if s.decision == DecisionProxy && nodeName != "" {
		n := c.nodes[nodeName]
		if n == nil {
			n = &NodeStat{Name: nodeName}
			c.nodes[nodeName] = n
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
	c.lifetime.addUpload(up)
	c.lifetime.addDownload(down)
	c.cur.Upload += up
	c.cur.Download += down
	switch d {
	case DecisionDirect:
		c.direct.addUpload(up)
		c.direct.addDownload(down)
		c.lifetimeDirect.addUpload(up)
		c.lifetimeDirect.addDownload(down)
		c.cur.DirectUpload += up
		c.cur.DirectDownload += down
	case DecisionProxy:
		c.proxy.addUpload(up)
		c.proxy.addDownload(down)
		c.lifetimeProxy.addUpload(up)
		c.lifetimeProxy.addDownload(down)
		c.cur.ProxyUpload += up
		c.cur.ProxyDownload += down
	}
	c.addRateLocked(nowSec, up, down, d)
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

// clearRateSlot zeroes one second of samples. All six arrays are cleared
// together: a slot that kept a stale split while its total was reset would
// report a share larger than the total for one second.
func (c *Collector) clearRateSlot(idx int) {
	c.rateUp[idx] = 0
	c.rateDown[idx] = 0
	c.rateUpDirect[idx] = 0
	c.rateDownDirect[idx] = 0
	c.rateUpProxy[idx] = 0
	c.rateDownProxy[idx] = 0
}

func (c *Collector) addRateLocked(nowSec, up, down int64, d Decision) {
	if c.rateSec != nowSec {
		if nowSec > c.rateSec {
			gap := nowSec - c.rateSec
			if gap >= rateSlots {
				c.rateUp = [rateSlots]int64{}
				c.rateDown = [rateSlots]int64{}
				c.rateUpDirect = [rateSlots]int64{}
				c.rateDownDirect = [rateSlots]int64{}
				c.rateUpProxy = [rateSlots]int64{}
				c.rateDownProxy = [rateSlots]int64{}
			} else {
				for i := int64(1); i <= gap; i++ {
					c.clearRateSlot(int((c.rateSec + i) % rateSlots))
				}
			}
		}
		c.rateSec = nowSec
	}
	idx := int(nowSec % rateSlots)
	c.rateUp[idx] += up
	c.rateDown[idx] += down
	switch d {
	case DecisionDirect:
		c.rateUpDirect[idx] += up
		c.rateDownDirect[idx] += down
	case DecisionProxy:
		c.rateUpProxy[idx] += up
		c.rateDownProxy[idx] += down
	}
	if c.rateUp[idx] > c.peakUp {
		c.peakUp = c.rateUp[idx]
	}
	if c.rateDown[idx] > c.peakDown {
		c.peakDown = c.rateDown[idx]
	}
	if c.rateUp[idx] > c.peakAllUp {
		c.peakAllUp = c.rateUp[idx]
	}
	if c.rateDown[idx] > c.peakAllDown {
		c.peakAllDown = c.rateDown[idx]
	}
}

// rateLocked sums the last window seconds, excluding the partially filled
// current second, and returns bytes per second.
func (c *Collector) rateLocked(nowSec int64, window int) Rate {
	if window <= 0 || window >= rateSlots {
		window = rateSlots - 1
	}
	var up, down, upDirect, downDirect, upProxy, downProxy int64
	for i := 1; i <= window; i++ {
		idx := int((nowSec - int64(i)) % rateSlots)
		if idx < 0 {
			idx += rateSlots
		}
		up += c.rateUp[idx]
		down += c.rateDown[idx]
		upDirect += c.rateUpDirect[idx]
		downDirect += c.rateDownDirect[idx]
		upProxy += c.rateUpProxy[idx]
		downProxy += c.rateDownProxy[idx]
	}
	w := int64(window)
	return Rate{
		Upload:         up / w,
		Download:       down / w,
		DirectUpload:   upDirect / w,
		DirectDownload: downDirect / w,
		ProxyUpload:    upProxy / w,
		ProxyDownload:  downProxy / w,
	}
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
		e.DirectBytes += up + down
	case DecisionProxy:
		e.ProxyConns += conns
		e.ProxyBytes += up + down
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
//
// ActiveDirect and ActiveProxy split the live sessions by the router's
// decision, the same domestic/foreign axis the traffic counters use. UDP is
// relayed direct-only and is not tracked as a session, so every live session
// carries one of the two decisions and the split always sums to ActiveConns.
type Overview struct {
	Now            int64        `json:"now"`
	StartTime      int64        `json:"start_time"`
	UptimeSeconds  int64        `json:"uptime_seconds"`
	ActiveConns    int          `json:"active_conns"`
	ActiveDirect   int          `json:"active_direct"`
	ActiveProxy    int          `json:"active_proxy"`
	Totals         Counters     `json:"totals"`
	Direct         Counters     `json:"direct"`
	Proxy          Counters     `json:"proxy"`
	Lifetime       Counters     `json:"lifetime"`        // never reset, survives restarts
	LifetimeDirect Counters     `json:"lifetime_direct"` // domestic share of lifetime
	LifetimeProxy  Counters     `json:"lifetime_proxy"`  // foreign share of lifetime
	Rate           Rate         `json:"rate"`            // last 5 seconds
	Rate1m         Rate         `json:"rate_1m"`         // last 60 seconds
	Peak           Rate         `json:"peak"`            // fastest second seen
	DNS            DNSStat      `json:"dns"`
	TopDomains     []DomainStat `json:"top_domains"`
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

	// One pass over the live sessions gives the domestic/foreign split of the
	// connection count, mirroring how every byte is attributed to a decision.
	activeDirect, activeProxy := 0, 0
	for _, s := range c.active {
		switch s.decision {
		case DecisionDirect:
			activeDirect++
		case DecisionProxy:
			activeProxy++
		}
	}

	o := Overview{
		Now:            nowSec,
		StartTime:      c.start.Unix(),
		UptimeSeconds:  int64(now.Sub(c.start) / time.Second),
		ActiveConns:    len(c.active),
		ActiveDirect:   activeDirect,
		ActiveProxy:    activeProxy,
		Totals:         c.totals,
		Direct:         c.direct,
		Proxy:          c.proxy,
		Lifetime:       c.lifetime,
		LifetimeDirect: c.lifetimeDirect,
		LifetimeProxy:  c.lifetimeProxy,
		Rate:           c.rateLocked(nowSec, 5),
		Rate1m:         c.rateLocked(nowSec, 60),
		Peak:           Rate{Upload: c.peakUp, Download: c.peakDown},
		DNS:            c.dnsSnapshotLocked(10),
		TopDomains:     c.topDomainsLocked(10, "bytes"),
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
		// Truncating by age alone would hide a long-running heavy download
		// behind newer idle connections: keep the busiest ones instead.
		type scored struct {
			s    *Session
			rate int64
		}
		sc := make([]scored, len(all))
		for i, s := range all {
			u, d := s.rates(now, s.upload.Load(), s.download.Load())
			sc[i] = scored{s, u + d}
		}
		sort.SliceStable(sc, func(i, j int) bool { return sc[i].rate > sc[j].rate })
		keep := sc[:opt.Limit]
		sort.Slice(keep, func(i, j int) bool { return keep[i].s.id > keep[j].s.id })
		all = all[:0]
		for _, k := range keep {
			all = append(all, k.s)
		}
	}
	out := make([]SessionView, 0, len(all))
	for _, s := range all {
		out = append(out, s.view(now))
	}
	return out
}

// SessionsNeedingProcess returns active TCP sessions that carry no process
// attribution yet and are young enough for their connection-table row to
// still exist — the retry feed for the GUI's attribution loop.
func (c *Collector) SessionsNeedingProcess(maxAge time.Duration) []SessionView {
	now := time.Now()
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]SessionView, 0, 8)
	for _, s := range c.active {
		if s.proto != ProtoSOCKS5 && s.proto != ProtoHTTP {
			continue
		}
		if s.Process() != "" || s.host == "" {
			continue
		}
		if now.Sub(s.start) > maxAge {
			continue
		}
		out = append(out, s.view(now))
	}
	return out
}

// SetSessionProcess records the attributed process on a live session.
func (c *Collector) SetSessionProcess(id int64, name, icon string) {
	c.mu.RLock()
	s, ok := c.active[id]
	c.mu.RUnlock()
	if ok {
		s.SetProcess(name, icon)
	}
}

// RecordUDPRelay counts relayed UDP bytes straight into the totals and the
// direct (domestic) path: UDP forwarding through upstreams is disabled by
// design, so every relayed datagram is a direct datagram. No per-session
// record is kept — UDP connections are not displayed anywhere.
func (c *Collector) RecordUDPRelay(up, down int64) {
	if c == nil {
		return
	}
	c.addTraffic(up, down, DecisionDirect)
}

// addProcess merges traffic into the per-process table. UDP datagrams carry
// no process and never reach here — the table is TCP-only by construction.
func (c *Collector) addProcess(name, icon string, d Decision, up, down int64) {
	c.mu.Lock()
	c.addProcessLocked(name, icon, d, up, down, 0)
	c.mu.Unlock()
}

func (c *Collector) addProcessLocked(name, icon string, dec Decision, up, down, conns int64) {
	now := time.Now()
	e := c.processes[name]
	if e == nil {
		e = &ProcessStat{Name: name, Icon: icon}
		c.processes[name] = e
	}
	if e.Icon == "" && icon != "" {
		e.Icon = icon
	}
	e.Upload += up
	e.Download += down
	e.Conns += conns
	switch dec {
	case DecisionDirect:
		e.DirectBytes += up + down
		e.directUp += up
		e.directDown += down
	case DecisionProxy:
		e.ProxyBytes += up + down
		e.proxyUp += up
		e.proxyDown += down
	}
	e.LastSeen = now.Unix()
}

// ResetProcesses clears the per-application table, leaving every other
// counter alone (the lifetime reset of a frontend showing both clears it
// alongside the domain table so the two cannot disagree).
func (c *Collector) ResetProcesses() {
	c.mu.Lock()
	c.processes = make(map[string]*ProcessStat)
	c.mu.Unlock()
}

// Processes returns the process table, ordered by bytes. Each call also
// advances the per-process rate sampling, so callers polling at a steady
// interval get speeds over that interval; rapid extra calls inside
// minRateSample reuse the previous sample.
func (c *Collector) Processes(limit int) []ProcessStat {
	c.mu.Lock()
	defer c.mu.Unlock()
	if limit <= 0 {
		limit = 100
	}
	now := time.Now()
	out := make([]ProcessStat, 0, len(c.processes))
	for _, d := range c.processes {
		d.sampleRate(now)
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total() == out[j].Total() {
			return out[i].Conns > out[j].Conns
		}
		return out[i].Total() > out[j].Total()
	})
	if len(out) > limit {
		out = out[:limit]
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

// BucketInterval is the resolution the history is stored at. A caller that
// asks for a finer step gets an alternating spike/zero series (one bucket's
// bytes land in one slot, the next slot is empty), so the chart code needs
// this to pick a step that matches the data it is reading.
func (c *Collector) BucketInterval() time.Duration {
	return c.cfg.BucketInterval
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
// reset cannot strand them. The lifetime counters are deliberately kept:
// they record the all-time traffic totals and only LifetimeReset clears them.
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
	c.rateUpDirect = [rateSlots]int64{}
	c.rateDownDirect = [rateSlots]int64{}
	c.rateUpProxy = [rateSlots]int64{}
	c.rateDownProxy = [rateSlots]int64{}
	c.peakUp = 0
	c.peakDown = 0
	c.start = now
}

// LifetimeReset clears only the permanent lifetime counters. Everything else
// is left alone.
func (c *Collector) LifetimeReset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lifetime = Counters{}
	c.lifetimeDirect = Counters{}
	c.lifetimeProxy = Counters{}
	c.peakAllUp = 0
	c.peakAllDown = 0
	c.maxConnsAll = 0
	c.activeSecAll = 0
}

// ResetDomains clears the per-destination traffic table. It touches neither
// the session counters nor the lifetime record: the domain table has its own
// scope (it survives restarts through the snapshot), and the lifetime reset
// of a frontend that shows both clears it too so the two cannot disagree.
func (c *Collector) ResetDomains() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.domains = make(map[string]*DomainStat)
}

// Lifetime returns the permanent traffic totals: all traffic plus its
// domestic (direct) and foreign (proxy) shares.
func (c *Collector) Lifetime() (all, direct, proxy Counters) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lifetime, c.lifetimeDirect, c.lifetimeProxy
}

// LifetimeStats returns the permanent totals as a struct, together with the
// lifetime monitoring metrics (peaks, max concurrency, on-time).
func (c *Collector) LifetimeStats() LifetimeStats {
	all, direct, proxy := c.Lifetime()
	c.mu.RLock()
	defer c.mu.RUnlock()
	return LifetimeStats{
		Totals:    all,
		Direct:    direct,
		Proxy:     proxy,
		PeakUp:    c.peakAllUp,
		PeakDown:  c.peakAllDown,
		MaxConns:  c.maxConnsAll,
		ActiveSec: c.activeSecAll,
	}
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
