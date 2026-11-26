package stats

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// newTestCollector builds a collector with fast buckets and no persistence.
func newTestCollector() *Collector {
	return New(Config{
		HistoryMinutes: 10,
		BucketInterval: time.Second,
		DomainLimit:    100,
		ErrorLimit:     10,
	})
}

func TestSessionAccounting(t *testing.T) {
	c := newTestCollector()

	s := c.Begin(ProtoSOCKS5, "127.0.0.1:5555", "example.com", 443, DecisionProxy, "node-a")
	if s == nil {
		t.Fatal("Begin returned nil session")
	}
	s.AddUpload(1000)
	s.AddDownload(4000)
	s.End(nil)

	ov := c.Overview()
	if ov.Totals.Upload != 1000 || ov.Totals.Download != 4000 {
		t.Fatalf("totals = %+v, want 1000/4000", ov.Totals)
	}
	if ov.Totals.Conns != 1 {
		t.Fatalf("conns = %d, want 1", ov.Totals.Conns)
	}
	if ov.Proxy.Upload != 1000 || ov.Direct.Download != 0 {
		t.Fatalf("split = proxy %+v direct %+v", ov.Proxy, ov.Direct)
	}
	if ov.ActiveConns != 0 {
		t.Fatalf("active = %d, want 0 after End", ov.ActiveConns)
	}

	// The domain table is flushed when the session ends.
	domains := c.Domains(10, "bytes")
	if len(domains) != 1 || domains[0].Domain != "example.com" {
		t.Fatalf("domains = %+v", domains)
	}
	if domains[0].Upload != 1000 || domains[0].Download != 4000 || domains[0].ProxyConns != 1 {
		t.Fatalf("domain stat = %+v", domains[0])
	}

	// Direct traffic lands in the direct counters.
	d := c.Begin(ProtoHTTP, "127.0.0.1:5556", "local.test", 80, DecisionDirect, "")
	d.AddUpload(10)
	d.End(nil)
	ov = c.Overview()
	if ov.Direct.Upload != 10 || ov.Direct.Conns != 1 {
		t.Fatalf("direct = %+v", ov.Direct)
	}
}

// The live connection count gets the same domestic/foreign split as the
// traffic counters: a session is counted once, under the decision the router
// made for it, and drops out of both when it ends.
func TestActiveConnsSplitByDecision(t *testing.T) {
	c := newTestCollector()

	direct := c.Begin(ProtoHTTP, "127.0.0.1:1", "cn.test", 80, DecisionDirect, "")
	proxy := c.Begin(ProtoSOCKS5, "127.0.0.1:2", "us.test", 443, DecisionProxy, "node-a")
	// A UDP association carries no single decision until its first datagram.
	udp := c.Begin(ProtoUDP, "127.0.0.1:3", "", 53, DecisionNone, "")

	ov := c.Overview()
	if ov.ActiveConns != 3 || ov.ActiveDirect != 1 || ov.ActiveProxy != 1 {
		t.Fatalf("active = %d (direct %d, proxy %d), want 3/1/1",
			ov.ActiveConns, ov.ActiveDirect, ov.ActiveProxy)
	}

	proxy.End(nil)
	ov = c.Overview()
	if ov.ActiveConns != 2 || ov.ActiveDirect != 1 || ov.ActiveProxy != 0 {
		t.Fatalf("after proxy end: active = %d (direct %d, proxy %d), want 2/1/0",
			ov.ActiveConns, ov.ActiveDirect, ov.ActiveProxy)
	}

	direct.End(nil)
	udp.End(nil)
	ov = c.Overview()
	if ov.ActiveConns != 0 || ov.ActiveDirect != 0 || ov.ActiveProxy != 0 {
		t.Fatalf("after all end: %+v, want zeroed split",
			[]int{ov.ActiveConns, ov.ActiveDirect, ov.ActiveProxy})
	}
}

func TestSessionDomainOnlyFillsAddressSessions(t *testing.T) {
	c := newTestCollector()

	// Dialled by address: the relay can learn the name from the traffic.
	byAddr := c.Begin(ProtoSOCKS5, "127.0.0.1:1", "112.49.67.179", 443, DecisionProxy, "")
	if !byAddr.WantsDomain() {
		t.Fatal("an IP literal session should want a sniffed name")
	}
	byAddr.SetDomain("static.zhihu.com")
	if got := byAddr.Domain(); got != "static.zhihu.com" {
		t.Fatalf("Domain = %q", got)
	}
	if byAddr.WantsDomain() {
		t.Fatal("a learned name should end the search")
	}

	// Dialled by name: the request is authoritative and must not be replaced.
	byName := c.Begin(ProtoSOCKS5, "127.0.0.1:2", "api.example.org", 443, DecisionProxy, "")
	if byName.WantsDomain() {
		t.Fatal("a named session needs no sniffing")
	}
	byName.SetDomain("wrong.example.net")
	if got := byName.Domain(); got != "" {
		t.Fatalf("named session was overwritten: %q", got)
	}

	// A UDP association multiplexes destinations; one name would be a lie.
	udp := c.Begin(ProtoUDP, "127.0.0.1:3", "", 53, DecisionNone, "")
	udp.SetDomain("dns.example.org")
	if got := udp.Domain(); got != "" {
		t.Fatalf("UDP association got a domain: %q", got)
	}

	// The view carries the name next to the address that was really used.
	found := false
	for _, v := range c.ActiveSessions(ActiveOptions{Limit: 10}) {
		if v.Host != "112.49.67.179" {
			continue
		}
		found = true
		if v.Domain != "static.zhihu.com" {
			t.Fatalf("view domain = %q, want static.zhihu.com", v.Domain)
		}
	}
	if !found {
		t.Fatal("session missing from the view")
	}

	// Once the session is over the name is no longer accepted.
	byAddr.End(nil)
	ended := byAddr
	ended.SetDomain("late.example.com")
	if got := ended.Domain(); got != "static.zhihu.com" {
		t.Fatalf("ended session took a late name: %q", got)
	}
}

func TestSessionIsIdempotentAndNilSafe(t *testing.T) {
	c := newTestCollector()
	s := c.Begin(ProtoHTTP, "1.2.3.4:1", "a.com", 80, DecisionDirect, "")
	s.AddUpload(5)
	s.End(nil)
	s.End(errors.New("late error")) // must not double count
	if got := c.Overview().Totals.Conns; got != 1 {
		t.Fatalf("conns = %d, want 1", got)
	}

	// A nil collector yields a nil session that accepts every call.
	var nilCollector *Collector
	ns := nilCollector.Begin(ProtoHTTP, "", "", 0, DecisionDirect, "")
	ns.AddUpload(1)
	ns.AddDownload(1)
	ns.AddDatagram("x", DecisionProxy, 1, 1)
	ns.End(nil)
	if ns.Upload() != 0 || ns.Download() != 0 {
		t.Fatal("nil session reported traffic")
	}
}

func TestSessionErrorRecordsError(t *testing.T) {
	c := newTestCollector()
	s := c.Begin(ProtoHTTP, "1.2.3.4:1", "bad.com", 443, DecisionProxy, "")
	s.Fail(errors.New("dial timeout"))

	if got := c.Overview().Totals.Errors; got != 1 {
		t.Fatalf("errors = %d, want 1", got)
	}
	errs := c.Errors(10)
	if len(errs) != 1 || errs[0].Scope != "proxy" || errs[0].Host != "bad.com" {
		t.Fatalf("errors = %+v", errs)
	}
	d := c.Domains(10, "bytes")
	if len(d) != 1 || d[0].Errors != 1 {
		t.Fatalf("domain errors not recorded: %+v", d)
	}
}

func TestActiveSessionsFilter(t *testing.T) {
	c := newTestCollector()
	a := c.Begin(ProtoSOCKS5, "10.0.0.1:1", "one.com", 443, DecisionProxy, "")
	b := c.Begin(ProtoHTTP, "10.0.0.2:2", "two.com", 80, DecisionDirect, "")
	defer a.End(nil)
	defer b.End(nil)

	if got := len(c.ActiveSessions(ActiveOptions{})); got != 2 {
		t.Fatalf("active = %d, want 2", got)
	}
	if got := c.ActiveSessions(ActiveOptions{Proto: "http"}); len(got) != 1 || got[0].Host != "two.com" {
		t.Fatalf("proto filter = %+v", got)
	}
	if got := c.ActiveSessions(ActiveOptions{Decision: "proxy"}); len(got) != 1 || got[0].Host != "one.com" {
		t.Fatalf("decision filter = %+v", got)
	}
	if got := c.ActiveSessions(ActiveOptions{Host: "ONE"}); len(got) != 1 {
		t.Fatalf("host filter should be case-insensitive: %+v", got)
	}
	if got := c.ActiveSessions(ActiveOptions{Limit: 1}); len(got) != 1 {
		t.Fatalf("limit = %d, want 1", len(got))
	}
}

func TestHistoryBucketsRotate(t *testing.T) {
	c := newTestCollector()

	s := c.Begin(ProtoHTTP, "1.1.1.1:1", "rotate.com", 80, DecisionProxy, "")
	s.AddUpload(100)
	s.End(nil)

	time.Sleep(1100 * time.Millisecond) // cross a one-second bucket boundary

	s2 := c.Begin(ProtoHTTP, "1.1.1.1:2", "rotate.com", 80, DecisionProxy, "")
	s2.AddDownload(200)
	s2.End(nil)

	points := c.Series(time.Now().Add(-time.Hour), time.Second)
	if len(points) < 2 {
		t.Fatalf("series has %d points, want at least 2 after a rotation", len(points))
	}
	var up, down int64
	for _, p := range points {
		up += p.Upload
		down += p.Download
	}
	if up != 100 || down != 200 {
		t.Fatalf("series totals up=%d down=%d, want 100/200", up, down)
	}
}

func TestSeriesFillsGapsAndCapsPoints(t *testing.T) {
	c := newTestCollector()
	s := c.Begin(ProtoHTTP, "", "gap.com", 80, DecisionDirect, "")
	s.AddUpload(7)
	s.End(nil)

	// A range far larger than the retention window must still be bounded.
	points := c.Series(time.Now().Add(-365*24*time.Hour), time.Second)
	if len(points) > maxSeriesPoints {
		t.Fatalf("series emitted %d points, cap is %d", len(points), maxSeriesPoints)
	}
	if len(points) == 0 {
		t.Fatal("series emitted no points")
	}
	// The last point carries the traffic we recorded.
	var up int64
	for _, p := range points {
		up += p.Upload
	}
	if up != 7 {
		t.Fatalf("series upload = %d, want 7", up)
	}
}

func TestDNSAndNodes(t *testing.T) {
	c := newTestCollector()
	c.RecordDNS("a.com", "A", "cache", nil)
	c.RecordDNS("a.com", "A", "doh", nil)
	c.RecordDNS("b.com", "AAAA", "direct", errors.New("upstream refused"))

	dns := c.DNSStats(10)
	if dns.Queries != 3 || dns.CacheHits != 1 || dns.DoH != 1 || dns.Direct != 1 || dns.Failures != 1 {
		t.Fatalf("dns = %+v", dns)
	}
	if dns.ByType["A"] != 2 || dns.ByType["AAAA"] != 1 {
		t.Fatalf("by_type = %+v", dns.ByType)
	}
	if len(dns.TopDomains) != 2 || dns.TopDomains[0].Domain != "a.com" {
		t.Fatalf("top domains = %+v", dns.TopDomains)
	}

	s := c.Begin(ProtoSOCKS5, "", "n.com", 443, DecisionProxy, "node-a")
	s.AddUpload(50)
	s.End(nil)
	nodes := c.Nodes()
	if len(nodes) != 1 || nodes[0].Name != "node-a" || nodes[0].Upload != 50 || nodes[0].Conns != 1 {
		t.Fatalf("nodes = %+v", nodes)
	}
}

func TestDomainTableIsCapped(t *testing.T) {
	c := New(Config{HistoryMinutes: 10, BucketInterval: time.Second, DomainLimit: 5})
	for i := 0; i < 20; i++ {
		s := c.Begin(ProtoHTTP, "", "d"+string(rune('a'+i))+".com", 80, DecisionDirect, "")
		s.AddUpload(int64(i + 1))
		s.End(nil)
	}
	if got := len(c.Domains(100, "bytes")); got > 5 {
		t.Fatalf("domain table holds %d entries, cap is 5", got)
	}
}

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "stats.json")

	c := New(Config{HistoryMinutes: 10, BucketInterval: time.Second, File: file, SaveInterval: 0})
	s := c.Begin(ProtoHTTP, "1.2.3.4:9", "persist.com", 443, DecisionProxy, "node-x")
	s.AddUpload(1234)
	s.AddDownload(5678)
	s.End(nil)
	c.RecordDNS("persist.com", "A", "doh", nil)
	if err := c.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded := New(Config{HistoryMinutes: 10, BucketInterval: time.Second, File: file, SaveInterval: 0})
	ov := loaded.Overview()
	// Session counters are per launch: the file carries them for the record,
	// but a new collector starts at zero. What has to survive is the lifetime
	// totals (the home page's "本次" cards are the ones that reset).
	if ov.Totals.Total() != 0 || ov.Totals.Conns != 0 || ov.Proxy.Upload != 0 {
		t.Fatalf("session counters restored from disk: totals=%+v proxy=%+v", ov.Totals, ov.Proxy)
	}
	if ov.Lifetime.Upload != 1234 || ov.Lifetime.Download != 5678 {
		t.Fatalf("restored lifetime = %+v", ov.Lifetime)
	}
	d := loaded.Domains(10, "bytes")
	if len(d) != 1 || d[0].Domain != "persist.com" || d[0].Download != 5678 {
		t.Fatalf("restored domains = %+v", d)
	}
	if got := loaded.DNSStats(5); got.Queries != 1 || got.DoH != 1 {
		t.Fatalf("restored dns = %+v", got)
	}
	if n := loaded.Nodes(); len(n) != 1 || n[0].Name != "node-x" {
		t.Fatalf("restored nodes = %+v", n)
	}
}

func TestLoadIgnoresCorruptFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "stats.json")
	if err := writeFile(file, "not json at all"); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := New(Config{HistoryMinutes: 5, BucketInterval: time.Second, File: file, SaveInterval: 0})
	if c.Overview().Totals.Conns != 0 {
		t.Fatal("corrupt snapshot should be ignored, not applied")
	}
}

func TestReset(t *testing.T) {
	c := newTestCollector()
	s := c.Begin(ProtoHTTP, "", "reset.com", 80, DecisionDirect, "")
	s.AddUpload(999)
	s.End(nil)
	c.Reset()
	ov := c.Overview()
	if ov.Totals.Upload != 0 || ov.Totals.Conns != 0 {
		t.Fatalf("after reset totals = %+v", ov.Totals)
	}
	if got := len(c.Domains(10, "bytes")); got != 0 {
		t.Fatalf("after reset domains = %d", got)
	}
}

func TestLifetimeSurvivesResetAndRestarts(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "stats.json")

	c := New(Config{HistoryMinutes: 10, BucketInterval: time.Second, File: file, SaveInterval: 0})
	direct := c.Begin(ProtoHTTP, "", "china.com", 80, DecisionDirect, "")
	direct.AddDownload(100)
	direct.End(nil)
	proxy := c.Begin(ProtoHTTP, "", "foreign.com", 443, DecisionProxy, "")
	proxy.AddDownload(300)
	proxy.End(nil)
	if err := c.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Reset clears the session counters but never the lifetime ones.
	c.Reset()
	all, lcDirect, lcProxy := c.Lifetime()
	if lcDirect.Download != 100 || lcProxy.Download != 300 || all.Total() != 400 {
		t.Fatalf("lifetime after reset = %+v %+v %+v", all, lcDirect, lcProxy)
	}

	// A restart restores the lifetime counters from the snapshot.
	restarted := New(Config{HistoryMinutes: 10, BucketInterval: time.Second, File: file, SaveInterval: 0})
	ov := restarted.Overview()
	if ov.Lifetime.Total() != 400 || ov.LifetimeDirect.Download != 100 || ov.LifetimeProxy.Download != 300 {
		t.Fatalf("restored lifetime = %+v %+v %+v", ov.Lifetime, ov.LifetimeDirect, ov.LifetimeProxy)
	}
	restarted.Close()
}

func TestLifetimeSeedsFromLegacySnapshot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "stats.json")

	// A snapshot written before lifetime counters existed: only the classic
	// totals are present.
	legacy := `{"version":1,"saved_at":"2026-01-01T00:00:00Z",
		"totals":{"upload_bytes":10,"download_bytes":90,"conns":2},
		"direct":{"download_bytes":90},"proxy":{"upload_bytes":10}}`
	if err := writeFile(file, legacy); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := New(Config{HistoryMinutes: 5, BucketInterval: time.Second, File: file, SaveInterval: 0})
	ov := c.Overview()
	if ov.Lifetime.Total() != 100 || ov.LifetimeDirect.Download != 90 || ov.LifetimeProxy.Upload != 10 {
		t.Fatalf("seeded lifetime = %+v %+v %+v", ov.Lifetime, ov.LifetimeDirect, ov.LifetimeProxy)
	}
	c.Close()

	// The standalone reader used by the GUI agrees.
	all, direct, proxy := LifetimeTotals(file)
	if all.Total() != 100 || direct.Download != 90 || proxy.Upload != 10 {
		t.Fatalf("LifetimeTotals = %+v %+v %+v", all, direct, proxy)
	}
}

func TestLifetimeResetStaysResetInV2Snapshot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "stats.json")

	// A version 2 snapshot where the session totals survived but the
	// lifetime counters were deliberately cleared: the old totals must NOT
	// be resurrected as lifetime.
	reset := `{"version":2,"saved_at":"2026-01-01T00:00:00Z",
		"totals":{"upload_bytes":10,"download_bytes":90,"conns":2},
		"lifetime":{"upload_bytes":0,"download_bytes":0},
		"lifetime_direct":{"download_bytes":0},
		"lifetime_proxy":{"upload_bytes":0}}`
	if err := writeFile(file, reset); err != nil {
		t.Fatalf("write: %v", err)
	}

	c := New(Config{HistoryMinutes: 5, BucketInterval: time.Second, File: file, SaveInterval: 0})
	ov := c.Overview()
	if ov.Lifetime.Total() != 0 {
		t.Fatalf("v2 lifetime = %+v, want zero", ov.Lifetime)
	}
	c.Close()

	all, _, _ := LifetimeTotals(file)
	if all.Total() != 0 {
		t.Fatalf("LifetimeTotals after reset = %+v, want zero", all)
	}
}

func TestUDPDatagrams(t *testing.T) {
	c := newTestCollector()
	s := c.Begin(ProtoUDP, "127.0.0.1:1", "", 0, DecisionNone, "")
	s.AddDatagram("udp.com", DecisionDirect, 120, 0)
	s.AddDatagram("", DecisionDirect, 0, 340)
	s.End(nil)

	ov := c.Overview()
	if ov.Totals.Upload != 120 || ov.Totals.Download != 340 {
		t.Fatalf("udp totals = %+v", ov.Totals)
	}
	if ov.Direct.Upload != 120 || ov.Direct.Download != 340 {
		t.Fatalf("udp split = %+v", ov.Direct)
	}
	d := c.Domains(10, "bytes")
	if len(d) != 1 || d[0].Domain != "udp.com" || d[0].Upload != 120 {
		t.Fatalf("udp domains = %+v", d)
	}
}

func TestSessionInstantaneousRate(t *testing.T) {
	c := New(Config{HistoryMinutes: 10})
	s := c.Begin(ProtoSOCKS5, "c", "dl.com", 443, DecisionProxy, "")
	defer s.End(nil)

	s.AddDownload(10 << 20)
	t0 := s.start.Add(time.Second)
	_, down := s.rates(t0, s.Upload(), s.Download())
	if down != 10<<20 {
		t.Fatalf("first sample = %d, want %d", down, 10<<20)
	}
	// Transfer stalls: the next interval must report ~0, not the lifetime average.
	_, down = s.rates(t0.Add(time.Second), s.Upload(), s.Download())
	if down != 0 {
		t.Fatalf("idle sample = %d, want 0", down)
	}
	// Rapid re-sample inside the minimum interval keeps the previous value.
	s.AddDownload(1 << 20)
	_, down2 := s.rates(t0.Add(time.Second+10*time.Millisecond), s.Upload(), s.Download())
	if down2 != 0 {
		t.Fatalf("rapid sample = %d, want previous 0", down2)
	}
}

func TestRateSplitByDecision(t *testing.T) {
	c := newTestCollector()
	now := time.Now().Unix()

	// Drive the sampling slots directly: the wall clock is not something a
	// test can wait for at one-second resolution.
	c.mu.Lock()
	c.addRateLocked(now-1, 600, 1200, DecisionDirect)
	c.addRateLocked(now-1, 400, 800, DecisionProxy)
	c.mu.Unlock()

	// Five-second window, so the sample is divided by five.
	r := c.rateLocked(now, 5)
	want := Rate{
		Upload: 200, Download: 400,
		DirectUpload: 120, DirectDownload: 240,
		ProxyUpload: 80, ProxyDownload: 160,
	}
	if r != want {
		t.Fatalf("rate = %+v, want %+v", r, want)
	}
	if r.DirectUpload+r.ProxyUpload != r.Upload || r.DirectDownload+r.ProxyDownload != r.Download {
		t.Fatalf("split does not add up: %+v", r)
	}

	// A gap clears the whole slot, split included: a stale share would read
	// as more traffic than the total it belongs to.
	c.mu.Lock()
	c.addRateLocked(now+10, 500, 0, DecisionProxy)
	c.mu.Unlock()
	if r := c.rateLocked(now+10, 5); r.Upload != 0 || r.DirectUpload != 0 || r.ProxyUpload != 0 {
		t.Fatalf("stale slots survived the gap: %+v", r)
	}
	if r := c.rateLocked(now+11, 5); r.Upload != 100 || r.ProxyUpload != 100 || r.DirectUpload != 0 {
		t.Fatalf("second after the gap = %+v", r)
	}

	c.Reset()
	if r := c.rateLocked(now+11, 5); r != (Rate{}) {
		t.Fatalf("rate survived Reset: %+v", r)
	}
}

// The chart reads the per-minute buckets, which already carried the split;
// this pins the aggregation so a refactor cannot silently drop it.
func TestSeriesKeepsDecisionSplit(t *testing.T) {
	c := newTestCollector()
	a := c.Begin(ProtoSOCKS5, "1.1.1.1:1", "cn.test", 80, DecisionDirect, "")
	a.AddUpload(100)
	a.AddDownload(200)
	b := c.Begin(ProtoSOCKS5, "1.1.1.1:2", "us.test", 80, DecisionProxy, "node")
	b.AddUpload(10)
	b.AddDownload(30)

	pts := c.Series(time.Now().Add(-time.Minute), time.Second)
	if len(pts) == 0 {
		t.Fatal("no series points")
	}
	var du, dd, pu, pd int64
	for _, p := range pts {
		du += p.DirectUpload
		dd += p.DirectDownload
		pu += p.ProxyUpload
		pd += p.ProxyDownload
	}
	if du != 100 || dd != 200 || pu != 10 || pd != 30 {
		t.Fatalf("split = %d/%d/%d/%d, want 100/200/10/30", du, dd, pu, pd)
	}
}

// The chart picks its step from this value: asking for slices finer than the
// stored buckets gives one spike followed by one empty slot, the rate reads
// double, and the curve turns into a sawtooth. Kept as a test because the
// getter is easy to delete as "unused" until the chart breaks.
func TestBucketIntervalReportsStoredResolution(t *testing.T) {
	c := newTestCollector()
	if got := c.BucketInterval(); got != time.Second {
		t.Fatalf("BucketInterval = %v, want %v", got, time.Second)
	}
	if got := New(Config{}).BucketInterval(); got != DefaultBucketInterval {
		t.Fatalf("default BucketInterval = %v, want %v", got, DefaultBucketInterval)
	}
}

// The home page's "this session" counters must start at zero: a number that
// silently carries on from the previous launch is indistinguishable from a
// number that counts all time, and the all-time figures already have their own
// card. The chart's history and the lifetime record still come off disk.
func TestLoadStartsSessionCountersAtZero(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		HistoryMinutes: 10,
		BucketInterval: time.Second,
		DomainLimit:    100,
		ErrorLimit:     10,
		File:           filepath.Join(dir, "stats.json"),
	}

	first := New(cfg)
	s := first.Begin(ProtoSOCKS5, "127.0.0.1:5000", "example.com", 443, DecisionDirect, "")
	s.AddUpload(1024)
	s.AddDownload(2048)
	// Let the bucket close, otherwise the traffic only exists in the in-flight
	// bucket, which is deliberately not persisted.
	time.Sleep(1100 * time.Millisecond)
	if ov := first.Overview(); ov.Totals.Upload != 1024 {
		t.Fatalf("first session totals = %+v", ov.Totals)
	}
	if err := first.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	second := New(cfg)
	ov := second.Overview()
	if ov.Totals.Upload != 0 || ov.Totals.Download != 0 {
		t.Fatalf("session totals restored from disk: %+v", ov.Totals)
	}
	if ov.Direct.Upload != 0 || ov.Proxy.Download != 0 {
		t.Fatalf("split restored from disk: direct=%+v proxy=%+v", ov.Direct, ov.Proxy)
	}
	if ov.Peak.Upload != 0 || ov.Peak.Download != 0 {
		t.Fatalf("peaks restored from disk: %+v", ov.Peak)
	}
	if ov.Lifetime.Upload != 1024 || ov.Lifetime.Download != 2048 {
		t.Fatalf("lifetime lost on reload: %+v", ov.Lifetime)
	}
	if pts := second.Series(time.Now().Add(-time.Minute), time.Second); len(pts) == 0 {
		t.Fatal("history lost on reload")
	}
}

// Version-1 files have no lifetime fields, so the lifetime counters are seeded
// from the totals they do carry. That seeding must read the file's totals, not
// the collector's (now empty) session counters.
func TestLoadSeedsLegacyLifetimeFromSnapshotTotals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")
	legacy := `{"version":1,"totals":{"upload_bytes":500,"download_bytes":700},` +
		`"direct":{"upload_bytes":400,"download_bytes":600},` +
		`"proxy":{"upload_bytes":100,"download_bytes":100},"dns":{"by_type":{}},"buckets":[]}`
	if err := writeFile(path, legacy); err != nil {
		t.Fatalf("write legacy file: %v", err)
	}

	c := New(Config{
		HistoryMinutes: 10,
		BucketInterval: time.Second,
		DomainLimit:    100,
		ErrorLimit:     10,
		File:           path,
	})
	ov := c.Overview()
	if ov.Lifetime.Upload != 500 || ov.Lifetime.Download != 700 {
		t.Fatalf("legacy lifetime seed = %+v, want 500/700", ov.Lifetime)
	}
	if ov.LifetimeDirect.Upload != 400 || ov.LifetimeProxy.Download != 100 {
		t.Fatalf("legacy split seed = %+v / %+v", ov.LifetimeDirect, ov.LifetimeProxy)
	}
	if ov.Totals.Upload != 0 || ov.Totals.Download != 0 {
		t.Fatalf("session totals inherited from a legacy file: %+v", ov.Totals)
	}
}
