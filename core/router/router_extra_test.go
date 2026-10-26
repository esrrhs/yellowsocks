package router

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// TestHostSetLenAndHas exercises the set primitives directly, including the
// zero-value set whose map has never been published.
func TestHostSetLenAndHas(t *testing.T) {
	var zero hostSet
	if zero.len() != 0 {
		t.Fatalf("zero-value set len = %d, want 0", zero.len())
	}
	if zero.has("anything.example") {
		t.Fatal("zero-value set must not match")
	}

	h := newHostSet()
	if h.len() != 0 {
		t.Fatalf("fresh set len = %d", h.len())
	}
	h.add("Example.com")
	if !h.has("example.com") {
		t.Fatal("added suffix must be found")
	}
	if h.has("other.com") {
		t.Fatal("missing suffix must not match")
	}
	if h.len() != 1 {
		t.Fatalf("len after one add = %d, want 1", h.len())
	}

	// A batch whose entries all trim to nothing publishes nothing and
	// leaves the set untouched.
	before := h.len()
	h.addAll([]string{"", "."})
	if h.len() != before {
		t.Fatalf("empty batch changed len from %d to %d", before, h.len())
	}
	// Leading/trailing dots and case are normalized on add.
	h.addAll([]string{".CN", ".Sub.Example."})
	if !h.has("cn") || !h.has("sub.example") {
		t.Fatalf("normalized batch missing entries: %v", h.rule.Load())
	}
}

// TestRouterInfo: the rule-set snapshot reports real counts and the default
// or configured skip country.
func TestRouterInfo(t *testing.T) {
	r := NewRouterWithOptions(Options{})
	defer r.Close()
	info := r.Info()
	if info.SkipCountry != "CN" {
		t.Fatalf("skip country = %q, want the CN default", info.SkipCountry)
	}
	if info.DirectDomains == 0 {
		t.Fatal("builtin direct domains missing from Info")
	}
	if info.ProxyDomains != 0 {
		t.Fatalf("proxy domains = %d, want 0", info.ProxyDomains)
	}
	if info.DirectCIDRs == 0 {
		t.Fatal("reserved CIDRs missing from Info")
	}
	if info.GeoIP {
		t.Fatal("GeoIP must be false without an mmdb file")
	}

	r.AddProxyDomain("blocked.example")
	r.AddDirectDomain("custom.example")
	baselineCIDRs := info.DirectCIDRs
	if err := r.AddDirectCIDR("203.0.113.0/24"); err != nil {
		t.Fatalf("add cidr: %v", err)
	}
	info = r.Info()
	if info.ProxyDomains != 1 {
		t.Fatalf("proxy domains = %d, want 1", info.ProxyDomains)
	}
	if info.DirectCIDRs != baselineCIDRs+1 {
		t.Fatalf("direct CIDRs = %d, want %d after one add", info.DirectCIDRs, baselineCIDRs+1)
	}
	if !r.ShouldDirectDomain("custom.example") {
		t.Fatal("custom direct domain should be listed")
	}

	// A custom skip country passes through.
	r2 := NewRouterWithOptions(Options{SkipCountry: "RU"})
	defer r2.Close()
	if got := r2.Info().SkipCountry; got != "RU" {
		t.Fatalf("skip country = %q, want RU", got)
	}
}

// TestNewRouterWithOptionsGeoIPFileMissing: a GeoIP file that cannot be
// opened is reported and skipped; the router still works.
func TestNewRouterWithOptionsGeoIPFileMissing(t *testing.T) {
	r := NewRouterWithOptions(Options{
		GeoIPFile: filepath.Join(t.TempDir(), "no-such.mmdb"),
	})
	defer r.Close()
	if r.Info().GeoIP {
		t.Fatal("GeoIP must be false after a failed load")
	}
	// Routing decisions still work without GeoIP.
	if r.Decide("", net.ParseIP("192.168.1.1")) != Direct {
		t.Fatal("reserved IP should still be direct")
	}
}

// TestNewRouterWithOptionsDefaultGeoIPFile: when the working directory holds
// GeoLite2-Country.mmdb it is picked up; an unparseable file must not crash
// the router and simply leaves GeoIP off.
func TestNewRouterWithOptionsDefaultGeoIPFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile("GeoLite2-Country.mmdb", []byte("not a real mmdb"), 0o644); err != nil {
		t.Fatalf("write stub mmdb: %v", err)
	}
	r := NewRouterWithOptions(Options{})
	defer r.Close()
	if r.Info().GeoIP {
		t.Fatal("garbage mmdb must not report GeoIP loaded")
	}
}

// TestNewRouterWithOptionsDomainFiles: options-driven domain files install
// into the right lists, and missing files are tolerated.
func TestNewRouterWithOptionsDomainFiles(t *testing.T) {
	dir := t.TempDir()
	chinaPath := filepath.Join(dir, "china.conf")
	gfwPath := filepath.Join(dir, "gfw.conf")
	if err := os.WriteFile(chinaPath, []byte("server=/file-direct.example/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gfwPath, []byte("server=/file-proxy.example/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRouterWithOptions(Options{
		DisableBuiltin:   true,
		ChinaDomainFiles: []string{chinaPath, filepath.Join(dir, "missing.conf")},
		GFWDomainFiles:   []string{gfwPath, filepath.Join(dir, "missing-gfw.conf")},
		DirectDomains:    []string{"opt-direct.example"},
		ProxyDomains:     []string{"opt-proxy.example"},
		DirectCIDRs:      []string{"198.51.100.0/24"},
	})
	defer r.Close()

	if !r.ShouldDirectDomain("www.file-direct.example") {
		t.Fatal("china domain file entry missing")
	}
	if !r.ShouldProxyDomain("sub.file-proxy.example") {
		t.Fatal("gfw domain file entry missing")
	}
	if !r.ShouldDirectDomain("opt-direct.example") {
		t.Fatal("options direct domain missing")
	}
	if !r.ShouldProxyDomain("opt-proxy.example") {
		t.Fatal("options proxy domain missing")
	}
	if !r.ShouldDirectIP(net.ParseIP("198.51.100.5")) {
		t.Fatal("options CIDR not installed")
	}
}

// TestShouldDirectIPBoundaries: nil IPs, routers without CIDR state, and
// IPv4-mapped spellings.
func TestShouldDirectIPBoundaries(t *testing.T) {
	// A router built without NewRouterWithOptions has no CIDR list at all.
	empty := &Router{}
	if empty.ShouldDirectIP(net.ParseIP("10.0.0.1")) {
		t.Fatal("router without CIDR state must not claim direct")
	}
	if empty.ShouldDirectIP(nil) {
		t.Fatal("nil IP must not be direct")
	}

	r := NewRouterWithOptions(Options{DisableBuiltin: true})
	defer r.Close()
	if r.ShouldDirectIP(nil) {
		t.Fatal("nil IP must not be direct")
	}
	// Reserved ranges are installed by default; the mapped form matches too.
	if !r.ShouldDirectIP(net.ParseIP("::ffff:192.168.1.1")) {
		t.Fatal("IPv4-mapped IPv6 should match the reserved v4 range")
	}
	if r.ShouldDirectIP(net.ParseIP("8.8.8.8")) {
		t.Fatal("public IP must not be direct")
	}
}

// TestNormalizeRulesTable: normalization drops junk rows, folds case, keeps
// the leading-dot suffix form, dedupes by kind+value with the first row's
// identity winning, and generates stable IDs.
func TestNormalizeRulesTable(t *testing.T) {
	in := []Rule{
		{Kind: " domain ", Value: "Example.COM", Action: " PROXY "}, // folds to domain/proxy/example.com
		{Kind: "app", Value: "Safari", Action: "proxy"},             // non-domain kinds dropped
		{Kind: "domain", Value: "   ", Action: "direct"},            // blank value dropped
		{Kind: "domain", Value: ".", Action: "direct"},              // dot-only value dropped
		{Kind: "DOMAIN", Value: ".Sub.Example.", Action: "bogus"},   // unknown action becomes direct
		{ID: "given-id", Kind: "domain", Value: "Pair.Example", Action: "direct"},
		{Kind: "domain", Value: "pair.example", Action: "proxy"}, // later duplicate wins, keeps given-id
		{Kind: "domain", Value: "Pair.example", Action: "proxy", Builtin: true},
		{Kind: "domain", Value: "a/b c.example", Action: "direct"}, // id generation escapes separators
	}

	out := NormalizeRules(in)
	var got []string
	for _, r := range out {
		got = append(got, r.Kind+"|"+r.Value+"|"+r.Action+"|"+r.ID)
	}
	want := []string{
		"domain|example.com|proxy|domain-example.com",
		"domain|.sub.example|direct|domain-.sub.example",
		"domain|pair.example|proxy|given-id",
		"domain|a/b c.example|direct|domain-a-b-c.example",
	}
	if len(got) != len(want) {
		t.Fatalf("normalized = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
	if !out[2].Builtin {
		t.Fatal("merging duplicates must OR the builtin flag")
	}

	// Normalizing nothing yields no rows.
	if out := NormalizeRules(nil); len(out) != 0 {
		t.Fatalf("nil rules -> %d rows", len(out))
	}
}

// TestMatchListedEdgeCases: empty and dot-only hosts never match any list.
func TestMatchListedEdgeCases(t *testing.T) {
	r := NewRouterWithOptions(Options{DisableBuiltin: true})
	defer r.Close()
	r.AddDirectDomain("example.com")
	r.AddProxyDomain("blocked.example")

	for _, host := range []string{"", ".", "..", " . "} {
		if r.ShouldDirectDomain(host) {
			t.Fatalf("ShouldDirectDomain(%q) must be false", host)
		}
		if r.ShouldProxyDomain(host) {
			t.Fatalf("ShouldProxyDomain(%q) must be false", host)
		}
	}
	// Case and surrounding dots on the query side fold to the stored form.
	if !r.ShouldDirectDomain(".EXAMPLE.com.") {
		t.Fatal("query-side folding failed")
	}
	// A decision for an unlisted host with no IP falls through to Proxy.
	if r.Decide("unlisted.example", nil) != Proxy {
		t.Fatal("unlisted host should proxy")
	}
}

// TestDecideHostBeatsIP: a listed domain decides even when the resolved IP
// would route the other way, and an IP literal in the host position is parsed.
func TestDecideHostBeatsIP(t *testing.T) {
	r := NewRouterWithOptions(Options{DisableBuiltin: true})
	defer r.Close()
	r.AddDirectDomain("direct.example")
	r.AddProxyDomain("proxy.example")

	// The domain rule wins over the destination IP.
	if got := r.Decide("direct.example", net.ParseIP("8.8.8.8")); got != Direct {
		t.Fatal("direct domain must decide despite proxy-ish IP")
	}
	if got := r.Decide("proxy.example", net.ParseIP("192.168.1.1")); got != Proxy {
		t.Fatal("proxy domain must decide despite direct-ish IP")
	}
	// An IP literal passed as the host is routed by IP rules.
	if got := r.Decide("192.168.1.1", nil); got != Direct {
		t.Fatal("IP literal host should be direct")
	}
	if got := r.Decide("8.8.8.8", nil); got != Proxy {
		t.Fatal("public IP literal host should proxy")
	}
	// A host that looks like an IP but is not stays a domain (proxy here).
	if got := r.Decide("1.2.3.4.5", nil); got != Proxy {
		t.Fatalf("near-IP host = %v, want Proxy", got)
	}
	// IPv6 literals in host form.
	if got := r.Decide("::1", nil); got != Direct {
		t.Fatal("::1 host should be direct")
	}
	if got := r.Decide("2001:4860:4860::8888", nil); got != Proxy {
		t.Fatal("public IPv6 host should proxy")
	}
	// Empty host and nil IP defaults to Proxy.
	if got := r.Decide("", nil); got != Proxy {
		t.Fatal("no information must default to Proxy")
	}
}

// TestAddAllBatchConsistency: addAll copies the previous set, so entries
// added earlier survive a later batch and vice versa.
func TestAddAllBatchConsistency(t *testing.T) {
	r := NewRouterWithOptions(Options{DisableBuiltin: true})
	defer r.Close()
	r.directHosts.addAll([]string{"one.example", "two.example"})
	r.directHosts.addAll([]string{"three.example"})
	for _, d := range []string{"one.example", "two.example", "three.example"} {
		if !r.ShouldDirectDomain(d) {
			t.Fatalf("%s lost after a later batch", d)
		}
	}
	if got := r.Info().DirectDomains; got != 3 {
		t.Fatalf("direct domains = %d, want 3", got)
	}
}
