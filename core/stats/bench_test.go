package stats

import (
	"testing"
)

// The relay calls AddUpload/AddDownload once per 32 KiB chunk, so this is
// the per-chunk tax every proxied connection pays.
func BenchmarkSessionAddUpload(b *testing.B) {
	c := New(Config{HistoryMinutes: 60})
	defer c.Close()
	s := c.Begin(ProtoSOCKS5, "127.0.0.1:51234", "example.com", 443, DecisionProxy, "node1")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.AddUpload(32 << 10)
	}
	b.StopTimer()
	s.End(nil)
}

// Same as above with many concurrent sessions: this is what a busy relay
// looks like, and it shows whether the collector lock serializes them.
func BenchmarkSessionAddUploadParallel(b *testing.B) {
	c := New(Config{HistoryMinutes: 60})
	defer c.Close()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		s := c.Begin(ProtoSOCKS5, "127.0.0.1:51234", "example.com", 443, DecisionProxy, "node1")
		for pb.Next() {
			s.AddUpload(32 << 10)
		}
		s.End(nil)
	})
}

// Begin/End is the per-connection cost: it touches the active session map.
func BenchmarkBeginEnd(b *testing.B) {
	c := New(Config{HistoryMinutes: 60})
	defer c.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := c.Begin(ProtoSOCKS5, "127.0.0.1:51234", "example.com", 443, DecisionProxy, "node1")
		s.End(nil)
	}
}
