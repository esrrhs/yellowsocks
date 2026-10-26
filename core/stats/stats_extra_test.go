package stats

import (
	"testing"
	"time"
)

// Constructor defaults and the simple accessors.
func TestNewDefaultsAndConfig(t *testing.T) {
	c := New(Config{})
	defer c.Close()
	cfg := c.Config()
	if cfg.HistoryMinutes != DefaultHistoryMinutes {
		t.Fatalf("HistoryMinutes default = %d", cfg.HistoryMinutes)
	}
	if cfg.BucketInterval != DefaultBucketInterval {
		t.Fatalf("BucketInterval default = %v", cfg.BucketInterval)
	}
	if cfg.DomainLimit != DefaultDomainLimit {
		t.Fatalf("DomainLimit default = %d", cfg.DomainLimit)
	}
	if cfg.ErrorLimit != DefaultErrorLimit {
		t.Fatalf("ErrorLimit default = %d", cfg.ErrorLimit)
	}
	if c.StartTime().IsZero() {
		t.Fatal("StartTime not set")
	}
}

func TestCountersAndDomainTotal(t *testing.T) {
	if got := (Counters{Upload: 1, Download: 2}).Total(); got != 3 {
		t.Fatalf("Counters.Total = %d, want 3", got)
	}
	if got := (DomainStat{Upload: 4, Download: 6}).Total(); got != 10 {
		t.Fatalf("DomainStat.Total = %d, want 10", got)
	}
}

func TestSessionNodeTracking(t *testing.T) {
	c := newTestCollector()
	defer c.Close()

	s := c.Begin(ProtoSOCKS5, "1.2.3.4:1", "node.com", 443, DecisionProxy, "")
	if s.Node() != "" {
		t.Fatalf("fresh session node = %q, want empty", s.Node())
	}
	s.SetNode("n1")
	if s.Node() != "n1" {
		t.Fatalf("node after SetNode = %q", s.Node())
	}
	s.SetNode("") // empty names are ignored
	if s.Node() != "n1" {
		t.Fatalf("node after empty SetNode = %q", s.Node())
	}
	s.SetNode("n2")
	if s.Node() != "n2" {
		t.Fatalf("node after second SetNode = %q", s.Node())
	}
	s.End(nil)

	// The node totals are committed on End.
	nodes := c.Nodes()
	if len(nodes) != 1 || nodes[0].Name != "n2" || nodes[0].Conns != 1 {
		t.Fatalf("nodes = %+v", nodes)
	}

	var nilSession *Session
	if nilSession.Node() != "" {
		t.Fatal("nil session reported a node")
	}
	nilSession.SetNode("x") // must not panic
}

func TestSessionZeroByteCallsAreNoops(t *testing.T) {
	c := newTestCollector()
	defer c.Close()

	s := c.Begin(ProtoHTTP, "1.2.3.4:1", "zero.com", 80, DecisionDirect, "")
	s.AddUpload(0)
	s.AddUpload(-5)
	s.AddDownload(0)
	if s.Upload() != 0 || s.Download() != 0 {
		t.Fatal("zero/negative adds were recorded")
	}
	s.flush() // nothing pending: must be a no-op
	s.AddDatagram("zero.com", DecisionNone, 0, 0)
	if s.Upload() != 0 || s.Download() != 0 {
		t.Fatal("empty datagram was recorded")
	}
	// Crossing the flush threshold commits bytes to the domain table while
	// the session is still open.
	s.AddUpload(flushThreshold)
	if s.Upload() != flushThreshold {
		t.Fatalf("upload after threshold = %d", s.Upload())
	}
	if d := c.Domains(10, "bytes"); len(d) != 1 || d[0].Domain != "zero.com" || d[0].Upload != flushThreshold {
		t.Fatalf("domains after threshold flush = %+v", d)
	}
	s.End(nil)

	// A direct zero-byte addTraffic call must not touch the counters either.
	c.addTraffic(0, 0, DecisionNone)
	ov := c.Overview()
	if ov.Totals.Upload != flushThreshold || ov.Totals.Download != 0 || ov.Totals.Conns != 1 {
		t.Fatalf("totals = %+v", ov.Totals)
	}
}

// rateLocked clamps the window, and a long stall wipes the per-second slots.
func TestRateWindowClampAndReset(t *testing.T) {
	c := newTestCollector()
	defer c.Close()

	now := time.Now().Unix()
	c.mu.Lock()
	c.rateUp[now%rateSlots] = 500
	c.rateDown[now%rateSlots] = 700

	// Window <= 0 and >= rateSlots both fall back to rateSlots-1.
	r := c.rateLocked(now, 0)
	if r.Upload != 0 || r.Download != 0 {
		t.Fatalf("rate with empty window = %+v", r)
	}
	r = c.rateLocked(now, rateSlots*2)
	if r.Upload != 0 || r.Download != 0 {
		t.Fatalf("rate with oversized window = %+v", r)
	}
	// Seconds before the epoch wrap to positive slot indexes.
	r = c.rateLocked(0, 5)
	if r.Upload != 0 || r.Download != 0 {
		t.Fatalf("rate at epoch = %+v", r)
	}

	// A jump of a full ring or more resets every slot before recording.
	c.addRateLocked(now+rateSlots+5, 10, 20)
	for i, v := range c.rateUp {
		if i != int((now+rateSlots+5)%rateSlots) && v != 0 {
			t.Fatalf("slot %d survived the reset: %d", i, v)
		}
	}
	if c.rateUp[(now+rateSlots+5)%rateSlots] != 10 {
		t.Fatalf("new traffic missing after reset: %+v", c.rateUp)
	}
	if c.peakUp < 10 || c.peakDown < 20 {
		t.Fatalf("peak = up %d down %d", c.peakUp, c.peakDown)
	}
	c.mu.Unlock()
}

func TestRotateAndBucketRing(t *testing.T) {
	// Sub-second bucket intervals fall back to the one-minute push step.
	c := New(Config{HistoryMinutes: 3, BucketInterval: 500 * time.Millisecond})
	defer c.Close()
	now := time.Now()
	c.mu.Lock()
	c.rotateLocked(now)
	first := c.cur.T
	c.rotateLocked(now.Add(3 * time.Minute))
	c.mu.Unlock()
	if first == 0 {
		t.Fatal("rotate did not open a current bucket")
	}
	if len(c.buckets) != 3 || c.buckets[2].T != first+120 {
		t.Fatalf("sub-second rotation pushed %d buckets: %+v", len(c.buckets), c.buckets)
	}

	// A collector whose current bucket was never stamped adopts now.
	c2 := newTestCollector()
	defer c2.Close()
	c2.mu.Lock()
	c2.cur = Bucket{}
	c2.rotateLocked(now)
	stamped := c2.cur.T
	c2.mu.Unlock()
	if stamped != alignUnix(now.Unix(), c2.cfg.BucketInterval) {
		t.Fatalf("empty current bucket not stamped: %d", stamped)
	}

	// Once the ring is full the oldest bucket falls off the front.
	c3 := New(Config{HistoryMinutes: 2, BucketInterval: time.Second})
	defer c3.Close()
	c3.mu.Lock()
	c3.pushBucketLocked(Bucket{T: 1})
	c3.pushBucketLocked(Bucket{T: 2})
	c3.pushBucketLocked(Bucket{T: 3}) // wraps
	ordered := c3.orderedBucketsLocked()
	// An unstamped bucket in the ring is skipped by the ordered view.
	c3.pushBucketLocked(Bucket{T: 0})
	orderedWithZero := c3.orderedBucketsLocked()
	c3.mu.Unlock()
	if len(ordered) != 2 || ordered[0].T != 2 || ordered[1].T != 3 {
		t.Fatalf("ring order after wrap = %+v", ordered)
	}
	// A zero (unstamped) slot replaces the oldest entry and the ordered view
	// skips it instead of emitting an empty bucket.
	if len(orderedWithZero) != 1 || orderedWithZero[0].T != 3 {
		t.Fatalf("zero bucket not skipped: %+v", orderedWithZero)
	}
}

func TestAlignUnixSubSecondInterval(t *testing.T) {
	if got := alignUnix(1234, 500*time.Millisecond); got != 1200 {
		t.Fatalf("alignUnix with sub-second interval = %d, want 1200", got)
	}
	if got := alignUnix(1259, 0); got != 1200 {
		t.Fatalf("alignUnix with zero interval = %d, want 1200", got)
	}
}

func TestRecordErrorAPI(t *testing.T) {
	c := newTestCollector()

	c.RecordError("engine", "h1", "boom %d", 42)
	c.RecordError("engine", "h2", "plain")
	for i := 0; i < 20; i++ {
		c.RecordError("socks5", "h3", "e%d", i)
	}
	errs := c.Errors(100)
	if len(errs) != 10 { // ErrorLimit of newTestCollector
		t.Fatalf("kept %d errors, want the limit 10", len(errs))
	}
	if errs[0].Message != "e10" || errs[9].Message != "e19" {
		t.Fatalf("oldest errors not dropped: %+v", errs)
	}
	if errs[9].Scope != "socks5" || errs[9].Host != "h3" {
		t.Fatalf("last error = %+v", errs[9])
	}
	if c.Overview().Totals.Errors != 22 {
		t.Fatalf("total errors = %d, want 22", c.Overview().Totals.Errors)
	}

	var nilCollector *Collector
	nilCollector.RecordError("engine", "h", "x") // must not panic
}

func TestRecordDNSBranches(t *testing.T) {
	c := newTestCollector()
	defer c.Close()

	// Empty query type and empty domain take the partial paths.
	c.RecordDNS("", "", "cache", nil)
	c.RecordDNS("", "A", "direct", nil)
	c.RecordDNS("x.com", "", "synthetic", nil)
	// A collector whose DNS maps were never initialized (as after a snapshot
	// with null maps) must recreate them on the first query. dnsNames is only
	// ever nil on a hand-built Collector, so give it what New would.
	zc := &Collector{dnsNames: map[string]*DomainStat{}}
	zc.RecordDNS("z.com", "A", "cache", nil)
	if zc.dns.ByType == nil || zc.dns.ByUpstream == nil {
		t.Fatal("nil DNS maps not re-created")
	}
	if zc.dns.Queries != 1 || len(zc.dnsNames) != 1 {
		t.Fatalf("zero-value collector state = %+v %+v", zc.dns, zc.dnsNames)
	}

	var nilCollector *Collector
	nilCollector.RecordDNS("a.com", "A", "cache", nil) // must not panic
}

// The DNS name table is pruned against its own limit, separate from domains.
func TestRecordDNSPrunesNames(t *testing.T) {
	c := New(Config{HistoryMinutes: 10, BucketInterval: time.Second, DomainLimit: 3})
	defer c.Close()
	for _, d := range []string{"a.com", "b.com", "c.com", "d.com", "e.com"} {
		c.RecordDNS(d, "A", "cache", nil)
	}
	if got := len(c.dnsNames); got > 3 {
		t.Fatalf("dns name table holds %d entries, cap is 3", got)
	}
	if got := c.DNSStats(2); len(got.TopDomains) != 2 {
		t.Fatalf("dns top domains = %+v, want 2", got.TopDomains)
	}
}

func TestTopDomainsByDecision(t *testing.T) {
	c := newTestCollector()
	defer c.Close()

	mk := func(host string, d Decision, up int64) {
		s := c.Begin(ProtoHTTP, "", host, 80, d, "")
		s.AddUpload(up)
		s.End(nil)
	}
	mk("direct.com", DecisionDirect, 100)
	mk("proxy1.com", DecisionProxy, 500)
	mk("proxy2.com", DecisionProxy, 300)
	direct := c.TopDomainsByDecision(10, DecisionDirect, "bytes")
	if len(direct) != 1 || direct[0].Domain != "direct.com" {
		t.Fatalf("direct = %+v", direct)
	}
	proxy := c.TopDomainsByDecision(10, DecisionProxy, "bytes")
	if len(proxy) != 2 || proxy[0].Domain != "proxy1.com" || proxy[1].Domain != "proxy2.com" {
		t.Fatalf("proxy = %+v", proxy)
	}
	byConns := c.TopDomainsByDecision(10, DecisionProxy, "conns")
	if len(byConns) != 2 {
		t.Fatalf("proxy by conns = %+v", byConns)
	}
	if got := c.TopDomainsByDecision(1, DecisionProxy, "bytes"); len(got) != 1 {
		t.Fatalf("limited = %+v", got)
	}
	// Domains with no conns on the requested path are excluded entirely.
	if got := c.TopDomainsByDecision(10, DecisionProxy, "conns"); len(got) != 2 {
		t.Fatalf("proxy conns = %+v", got)
	}
}

func TestDomainsOrderingAndTies(t *testing.T) {
	c := newTestCollector()
	defer c.Close()

	add := func(host string, up int64, times int) {
		for i := 0; i < times; i++ {
			s := c.Begin(ProtoHTTP, "", host, 80, DecisionDirect, "")
			s.AddUpload(up)
			s.End(nil)
		}
	}
	add("many-small.com", 10, 3) // 30 bytes, 3 conns
	add("one-big.com", 100, 1)   // 100 bytes, 1 conn
	add("tie-a.com", 25, 2)      // 50 bytes, 2 conns
	add("tie-b.com", 50, 1)      // 50 bytes, 1 conn

	byConns := c.Domains(0, "conns")
	if len(byConns) != 4 || byConns[0].Domain != "many-small.com" {
		t.Fatalf("by conns = %+v", byConns)
	}
	byBytes := c.Domains(2, "bytes")
	if len(byBytes) != 2 || byBytes[0].Domain != "one-big.com" {
		t.Fatalf("by bytes limited = %+v", byBytes)
	}
	// Equal totals tie-break on connections, whichever order is requested.
	ties := c.Domains(10, "bytes")
	if ties[1].Domain != "tie-a.com" || ties[2].Domain != "tie-b.com" {
		t.Fatalf("tie order = %+v", ties)
	}
	tiesConns := c.Domains(10, "conns")
	if tiesConns[1].Domain != "tie-a.com" || tiesConns[2].Domain != "one-big.com" {
		t.Fatalf("tie order by conns = %+v", tiesConns)
	}
}

func TestNodesOrdering(t *testing.T) {
	c := newTestCollector()
	defer c.Close()

	finish := func(host, node string, up int64) {
		s := c.Begin(ProtoSOCKS5, "", host, 443, DecisionProxy, node)
		s.AddUpload(up)
		s.End(nil)
	}
	finish("a.com", "small-node", 10)
	finish("b.com", "big-node", 999)

	nodes := c.Nodes()
	if len(nodes) != 2 || nodes[0].Name != "big-node" || nodes[1].Name != "small-node" {
		t.Fatalf("nodes = %+v", nodes)
	}
}

func TestTopFromLockedTruncationAndTies(t *testing.T) {
	c := newTestCollector()
	defer c.Close()
	for _, d := range []string{"n1.com", "n2.com", "n3.com", "n4.com", "n5.com"} {
		c.RecordDNS(d, "A", "cache", nil)
	}
	top := c.DNSStats(3).TopDomains
	if len(top) != 3 {
		t.Fatalf("top = %+v, want 3", top)
	}
	all := c.DNSStats(10).TopDomains
	if len(all) != 5 {
		t.Fatalf("top with topN=10 = %+v, want all 5", all)
	}
}

func TestSeriesEdgeCases(t *testing.T) {
	c := newTestCollector()
	defer c.Close()
	s := c.Begin(ProtoHTTP, "", "series.com", 80, DecisionDirect, "")
	s.AddUpload(11)
	s.End(nil)

	// A since in the future covers an empty range.
	if pts := c.Series(time.Now().Add(time.Hour), time.Second); pts != nil {
		t.Fatalf("future series = %+v, want nil", pts)
	}
	// step <= 0 falls back to the bucket interval (1s here).
	pts := c.Series(time.Now().Add(-2*time.Second), 0)
	if len(pts) == 0 {
		t.Fatal("default-step series emitted no points")
	}
	// Sub-second steps still cover whole seconds (clamped to 60s buckets).
	pts = c.Series(time.Now().Add(-2*time.Second), 500*time.Millisecond)
	if len(pts) == 0 {
		t.Fatal("sub-second-step series emitted no points")
	}
	var up int64
	for _, p := range pts {
		up += p.Upload
	}
	if up != 11 {
		t.Fatalf("series upload = %d, want 11", up)
	}
}

func TestHistoryWindow(t *testing.T) {
	c := newTestCollector()
	defer c.Close()

	// Without closed buckets the window is just the current one: it starts at
	// the collector's creation, aligned to the bucket interval.
	oldest, newest := c.HistoryWindow()
	if oldest.Before(c.start.Add(-2*time.Second)) || oldest.After(time.Now()) {
		t.Fatalf("fresh window oldest = %v, start = %v", oldest, c.start)
	}
	if newest.Before(oldest) {
		t.Fatalf("fresh window newest %v before oldest %v", newest, oldest)
	}

	now := time.Now()
	c.mu.Lock()
	c.pushBucketLocked(Bucket{T: now.Add(-2 * time.Hour).Unix()})
	c.cur.T = alignUnix(now.Unix(), c.cfg.BucketInterval)
	c.mu.Unlock()
	oldest, newest = c.HistoryWindow()
	if oldest.Unix() != now.Add(-2*time.Hour).Unix() {
		t.Fatalf("window oldest = %v, want two hours ago", oldest)
	}
	if newest.Before(oldest.Add(time.Hour)) {
		t.Fatalf("window newest = %v, want the current bucket", newest)
	}

	// A collector with no buckets at all and no current bucket falls back to
	// its own lifetime. cur is only ever zero before the first traffic, so it
	// is cleared here by hand.
	c2 := newTestCollector()
	defer c2.Close()
	c2.mu.Lock()
	c2.cur = Bucket{}
	c2.mu.Unlock()
	oldest, newest = c2.HistoryWindow()
	if !oldest.Equal(c2.start) {
		t.Fatalf("bucket-less window oldest = %v, want start %v", oldest, c2.start)
	}
	if newest.Before(oldest) {
		t.Fatalf("bucket-less window newest %v before oldest %v", newest, oldest)
	}
}

func TestContainsFold(t *testing.T) {
	if !containsFold("Hello.COM", "hello") {
		t.Fatal("case-insensitive match failed")
	}
	if !containsFold("anything", "") {
		t.Fatal("empty substring must match everything")
	}
	if containsFold("abc", "zzz") {
		t.Fatal("false positive")
	}
}

// When the live list is over its limit the busiest sessions are kept, not the
// newest, and the re-ranked result is sorted newest first again.
func TestActiveSessionsKeepsBusiest(t *testing.T) {
	c := New(Config{HistoryMinutes: 10})
	defer c.Close()

	var sessions []*Session
	for i := 0; i < 5; i++ {
		s := c.Begin(ProtoSOCKS5, "c", "busy.com", 443, DecisionProxy, "")
		s.AddUpload(int64(i+1) * 1000)
		sessions = append(sessions, s)
	}
	defer func() {
		for _, s := range sessions {
			s.End(nil)
		}
	}()

	got := c.ActiveSessions(ActiveOptions{Limit: 3})
	if len(got) != 3 {
		t.Fatalf("active = %d, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].ID <= got[i].ID {
			t.Fatalf("not sorted newest first: %+v", got)
		}
	}
}
