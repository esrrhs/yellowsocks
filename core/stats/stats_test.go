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
	if ov.Totals.Upload != 1234 || ov.Totals.Download != 5678 || ov.Totals.Conns != 1 {
		t.Fatalf("restored totals = %+v", ov.Totals)
	}
	if ov.Proxy.Upload != 1234 {
		t.Fatalf("restored proxy split = %+v", ov.Proxy)
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
