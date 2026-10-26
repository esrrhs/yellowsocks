package api

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/yellowsocks/core/stats"
)

// mustURL parses a URL or fails the test; it keeps the request-building
// assertions terse.
func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

// TestAuthorized exercises the admin-token-only check directly: the readonly
// token must not satisfy it even though it passes accessLevel for GETs.
func TestAuthorized(t *testing.T) {
	c := stats.New(stats.Config{})
	s, err := New(Config{Token: "admin", ReadonlyToken: "ro"}, Deps{Stats: c})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil)
	if s.authorized(r) {
		t.Fatal("request without a token must not be authorized")
	}
	r.Header.Set("Authorization", "Bearer admin")
	if !s.authorized(r) {
		t.Fatal("admin bearer token must be authorized")
	}
	r.Header.Set("Authorization", "Bearer ro")
	if s.authorized(r) {
		t.Fatal("readonly token must not pass the admin check")
	}
	r.Header.Del("Authorization")
	r.Header.Set("X-API-Token", "admin")
	if !s.authorized(r) {
		t.Fatal("admin X-API-Token must be authorized")
	}
	r.Header.Del("X-API-Token")
	r.URL = mustURL(t, "/api/v1/overview?token=admin")
	if !s.authorized(r) {
		t.Fatal("admin query token must be authorized")
	}
	r.URL = mustURL(t, "/api/v1/overview?token=nope")
	if s.authorized(r) {
		t.Fatal("wrong query token must not be authorized")
	}
}

// newTestServerLive is newTestServer with an additional active (not ended)
// session so /api/v1/connections has something to report, while the ended
// seeded session feeds the domain/overview aggregates.
func newTestServerLive(t *testing.T, token string) (*Server, string, *stats.Collector) {
	t.Helper()
	c := stats.New(stats.Config{HistoryMinutes: 60, BucketInterval: time.Minute})
	s := c.Begin(stats.ProtoHTTP, "10.0.0.1:1234", "example.com", 443, stats.DecisionProxy, "node-a")
	s.AddUpload(2048)
	s.AddDownload(8192)
	s.End(nil)
	live := c.Begin(stats.ProtoHTTP, "10.0.0.2:1234", "example.com", 443, stats.DecisionProxy, "node-a")
	c.RecordDNS("example.com", "A", "doh", nil)
	c.RecordError("http", "bad.com", "upstream refused")

	srv, err := New(Config{ListenAddr: "127.0.0.1:0", Token: token, Dashboard: true},
		Deps{Stats: c, Info: fakeInfo{}, Version: "test-1.0"})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		live.End(nil)
		_ = srv.Stop()
	})
	return srv, "http://" + srv.Addr().String(), c
}

// TestHandleConnections covers the /api/v1/connections handler: the live
// session is listed, each query filter narrows it, and the limit parameter is
// clamped instead of trusted.
func TestHandleConnections(t *testing.T) {
	_, base, _ := newTestServerLive(t, "secret")

	var out ConnectionsResponse
	getJSON(t, base+"/api/v1/connections", "secret", &out)
	if out.Count != 1 || len(out.Connections) != 1 {
		t.Fatalf("connections = %d entries, want the one live session (%+v)", out.Count, out.Connections)
	}
	if out.Connections[0].Host != "example.com" {
		t.Fatalf("connection host = %q", out.Connections[0].Host)
	}

	// Each filter that matches keeps the session...
	for _, q := range []string{"proto=http", "decision=proxy", "host=example", "host=EXAMPLE"} {
		var filtered ConnectionsResponse
		getJSON(t, base+"/api/v1/connections?"+q, "secret", &filtered)
		if filtered.Count != 1 {
			t.Fatalf("filter %q -> %d entries, want 1", q, filtered.Count)
		}
	}
	// ...and one that does not match drops it.
	var none ConnectionsResponse
	getJSON(t, base+"/api/v1/connections?host=other.example", "secret", &none)
	if none.Count != 0 {
		t.Fatalf("non-matching host filter -> %d entries, want 0", none.Count)
	}
	// An invalid limit falls back to the default rather than failing.
	var def ConnectionsResponse
	getJSON(t, base+"/api/v1/connections?limit=abc", "secret", &def)
	if def.Count != 1 {
		t.Fatalf("bad limit -> %d entries, want 1", def.Count)
	}
}

// TestHandleDomains covers the /api/v1/domains handler: rows are returned,
// both orderings are accepted, and anything else falls back to bytes.
func TestHandleDomains(t *testing.T) {
	_, base, _ := newTestServerLive(t, "secret")

	for _, order := range []string{"bytes", "conns", "bogus"} {
		var out DomainsResponse
		getJSON(t, base+"/api/v1/domains?order="+order+"&limit=10", "secret", &out)
		if out.Count != 1 || len(out.Domains) != 1 {
			t.Fatalf("order=%s -> %+v, want the seeded domain", order, out.Domains)
		}
		if out.Domains[0].Domain != "example.com" {
			t.Fatalf("order=%s -> domain %q", order, out.Domains[0].Domain)
		}
	}

	// Limit clamps: below the floor and above the ceiling both succeed.
	var clamped DomainsResponse
	getJSON(t, base+"/api/v1/domains?limit=-1", "secret", &clamped)
	if clamped.Count != 1 {
		t.Fatalf("limit=-1 -> %+v", clamped)
	}
	getJSON(t, base+"/api/v1/domains?limit=999999", "secret", &clamped)
}

// TestHandleErrorsTruncation covers the tail-truncation branch of
// /api/v1/errors: more entries than the requested limit keeps only the last.
func TestHandleErrorsTruncation(t *testing.T) {
	_, base, c := newTestServerLive(t, "secret")
	for i := 0; i < 5; i++ {
		c.RecordError("socks5", "err.example", "failure %d", i)
	}

	var out ErrorsResponse
	getJSON(t, base+"/api/v1/errors?limit=2", "secret", &out)
	if out.Count != 2 || len(out.Errors) != 2 {
		t.Fatalf("limit=2 -> %d entries, want 2", out.Count)
	}
	if !strings.Contains(out.Errors[len(out.Errors)-1].Message, "4") {
		t.Fatalf("truncation must keep the newest entries: %+v", out.Errors)
	}
	// Uppercase scope values are matched case-insensitively.
	var scoped ErrorsResponse
	getJSON(t, base+"/api/v1/errors?scope=SOCKS5&limit=10", "secret", &scoped)
	if scoped.Count != 5 {
		t.Fatalf("scope=SOCKS5 -> %d entries, want 5", scoped.Count)
	}
}

// TestClientIP walks the X-Forwarded-For / X-Real-IP resolution rules against
// a trusted proxy peer, plus the untrusted fallbacks.
func TestClientIP(t *testing.T) {
	trusted := func() *Server {
		s, err := New(Config{
			Token:          "t",
			TrustedProxies: []string{"127.0.0.1/32", "10.1.0.0/16"},
		}, Deps{Stats: stats.New(stats.Config{})})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		return s
	}
	reqFrom := func(remote string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil)
		r.RemoteAddr = remote
		return r
	}

	s := trusted()

	// An untrusted peer's headers are ignored entirely.
	r := reqFrom("192.0.2.9:5555")
	r.Header.Set("X-Forwarded-For", "203.0.113.5")
	if got := s.clientIP(r); !got.Equal(net.ParseIP("192.0.2.9")) {
		t.Fatalf("untrusted peer XFF = %v, want 192.0.2.9", got)
	}
	// Unparseable RemoteAddr yields nil rather than panicking.
	if got := s.clientIP(reqFrom("not-an-addr")); got != nil {
		t.Fatalf("bad RemoteAddr = %v, want nil", got)
	}

	// A trusted proxy's chain is walked right to left: the last untrusted hop
	// is the client.
	r = reqFrom("127.0.0.1:1000")
	r.Header.Set("X-Forwarded-For", "203.0.113.5, 10.1.2.3, 10.1.2.4")
	if got := s.clientIP(r); !got.Equal(net.ParseIP("203.0.113.5")) {
		t.Fatalf("trusted XFF chain = %v, want 203.0.113.5", got)
	}
	// Every hop is itself trusted: fall back to the first entry in the chain.
	r.Header.Set("X-Forwarded-For", "10.1.2.3, 127.0.0.1")
	if got := s.clientIP(r); !got.Equal(net.ParseIP("10.1.2.3")) {
		t.Fatalf("all-trusted XFF = %v, want 10.1.2.3", got)
	}
	// The chain holds nothing parseable: X-Real-IP is consulted next.
	r.Header.Set("X-Forwarded-For", "garbage, ,, junk")
	r.Header.Set("X-Real-IP", "198.51.100.7")
	if got := s.clientIP(r); !got.Equal(net.ParseIP("198.51.100.7")) {
		t.Fatalf("XFF junk + X-Real-IP = %v, want 198.51.100.7", got)
	}
	// Neither header is usable: the peer itself is the client.
	r.Header.Del("X-Forwarded-For")
	r.Header.Set("X-Real-IP", "not-an-ip")
	if got := s.clientIP(r); !got.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("bad X-Real-IP = %v, want peer 127.0.0.1", got)
	}
	// A bare X-Real-IP from the trusted proxy wins.
	r.Header.Del("X-Real-IP")
	r.Header.Set("X-Real-IP", "198.51.100.7")
	if got := s.clientIP(r); !got.Equal(net.ParseIP("198.51.100.7")) {
		t.Fatalf("bare X-Real-IP = %v, want 198.51.100.7", got)
	}

	// Without configured trusted proxies the headers never matter.
	s2, err := New(Config{Token: "t"}, Deps{Stats: stats.New(stats.Config{})})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	r = reqFrom("127.0.0.1:1000")
	r.Header.Set("X-Forwarded-For", "203.0.113.5")
	if got := s2.clientIP(r); !got.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("no trusted proxies: XFF = %v, want peer", got)
	}
}

// TestParseIPFromAddrAndInNets are small pure helpers; cover the host:port,
// bare-host and IPv6 spellings plus the nil-IP guard.
func TestParseIPFromAddrAndInNets(t *testing.T) {
	if got := parseIPFromAddr("10.1.2.3:8080"); !got.Equal(net.ParseIP("10.1.2.3")) {
		t.Fatalf("host:port = %v", got)
	}
	if got := parseIPFromAddr("10.1.2.3"); !got.Equal(net.ParseIP("10.1.2.3")) {
		t.Fatalf("bare host = %v", got)
	}
	if got := parseIPFromAddr("[2001:db8::1]:443"); !got.Equal(net.ParseIP("2001:db8::1")) {
		t.Fatalf("ipv6 host:port = %v", got)
	}
	if got := parseIPFromAddr(""); got != nil {
		t.Fatalf("empty addr = %v, want nil", got)
	}
	// An IPv4 parse returns the 4-byte form.
	if got := parseIPFromAddr("1.2.3.4:80"); got.To4() == nil || len(got) != 4 {
		t.Fatalf("ipv4 should come back 4-byte, got %v (%d bytes)", got, len(got))
	}

	if inNets(nil, nil) {
		t.Fatal("nil IP is in no net")
	}
	_, v4net, _ := net.ParseCIDR("10.0.0.0/8")
	if !inNets(net.ParseIP("10.1.2.3"), []*net.IPNet{v4net}) {
		t.Fatal("10.1.2.3 should be in 10.0.0.0/8")
	}
	// IPv4-mapped IPv6 is canonicalized before the check.
	if !inNets(net.ParseIP("::ffff:10.1.2.3"), []*net.IPNet{v4net}) {
		t.Fatal("mapped IPv4 should match the v4 net")
	}
	if inNets(net.ParseIP("11.1.2.3"), []*net.IPNet{v4net}) {
		t.Fatal("11.1.2.3 is outside 10.0.0.0/8")
	}
}

// TestIntParamClamps: unset falls back, garbage falls back, out-of-range is
// clamped into [min, max].
func TestIntParamClamps(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", 50},
		{"abc", 50},
		{"-5", 1},
		{"0", 1},
		{"1", 1},
		{"25", 25},
		{"100000", MaxDomainLimit},
	}
	for _, tc := range cases {
		if got := intParam(tc.raw, 50, 1, MaxDomainLimit); got != tc.want {
			t.Fatalf("intParam(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

// TestParseSpanAndStepEdgeCases: the duration parser has a handful of
// boundary forms the endpoint tests never hit.
func TestParseSpanAndStepEdgeCases(t *testing.T) {
	def := time.Hour
	if got := parseSpan("0", def); got != def {
		t.Fatalf("parseSpan(0) = %v, want the default", got)
	}
	if got := parseSpan("-5m", def); got != def {
		t.Fatalf("parseSpan(-5m) = %v, want the default", got)
	}
	if got := parseSpan("400d", def); got != 365*24*time.Hour {
		t.Fatalf("parseSpan(400d) = %v, want a one-year clamp", got)
	}
	if got := parseSpan("2h", def); got != 2*time.Hour {
		t.Fatalf("parseSpan(2h) = %v", got)
	}

	if got, err := parseDuration(" 90S "); err != nil || got != 90*time.Second {
		t.Fatalf("parseDuration should trim and fold case, got %v, %v", got, err)
	}
	if _, err := parseDuration(""); err == nil {
		t.Fatal("empty duration must error")
	}
	if _, err := parseDuration("5x"); err == nil {
		t.Fatal("unknown suffix must error")
	}
	// Fractional and multi-unit Go durations ("1.5h", "1h30m") are not
	// supported by the compact parser: it strips the trailing unit and runs
	// Atoi on the rest. Callers fall back to the default span.
	if _, err := parseDuration("1.5h"); err == nil {
		t.Fatal("fractional compact durations are rejected; update this test if the parser gains Go-duration support")
	}
	if got := parseSpan("1.5h", def); got != def {
		t.Fatalf("parseSpan(1.5h) = %v, want the default fallback", got)
	}

	// parseStep: an explicit valid step wins; an invalid one falls through to
	// the automatic ladder.
	if got := parseStep("5m", time.Hour); got != 5*time.Minute {
		t.Fatalf("parseStep(5m) = %v", got)
	}
	if got := parseStep("nonsense", time.Hour); got != time.Minute {
		t.Fatalf("parseStep(nonsense, 1h) = %v, want 1m", got)
	}
	if got := parseStep("", 365*24*time.Hour); got != 24*time.Hour {
		t.Fatalf("parseStep(1y) = %v, want the 24h ceiling", got)
	}
}

// TestWriteJSONMarshalError: a value JSON cannot represent must come back as a
// 500 with an error body rather than a half-written response.
func TestWriteJSONMarshalError(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]interface{}{"bad": make(chan int)})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("writeJSON(chan) = %d, want 500", w.Code)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if _, ok := out["error"]; !ok {
		t.Fatalf("error body missing the error field: %s", w.Body.String())
	}

	// The happy path sets the hardening headers.
	w = httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]string{"ok": "yes"})
	if w.Code != http.StatusOK {
		t.Fatalf("writeJSON ok = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content type = %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache control = %q", cc)
	}
	if xo := w.Header().Get("X-Content-Type-Options"); xo != "nosniff" {
		t.Fatalf("nosniff = %q", xo)
	}
}

// TestCORSAllowedOriginList: with an explicit origin list the header echoes a
// matching origin and is withheld from everything else.
func TestCORSAllowedOriginList(t *testing.T) {
	_, base := startServerOn(t, Config{Token: "t", AllowedOrigins: []string{"https://good.example"}})

	// Matching origin: echoed verbatim.
	resp := doRequest(t, http.MethodGet, base+"/api/v1/overview?token=t", map[string]string{"Origin": "https://good.example"})
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://good.example" {
		t.Fatalf("allow-origin = %q, want the echoed origin", got)
	}
	if v := resp.Header.Get("Vary"); v != "Origin" {
		t.Fatalf("vary = %q", v)
	}

	// Non-matching origin: no CORS headers at all.
	resp = doRequest(t, http.MethodGet, base+"/api/v1/overview?token=t", map[string]string{"Origin": "https://evil.example"})
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("foreign origin got allow-origin %q", got)
	}

	// No Origin header (curl/server-to-server): CORS is skipped.
	resp = doRequest(t, http.MethodGet, base+"/api/v1/overview?token=t", nil)
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("originless request got allow-origin %q", got)
	}

	// A preflight from a foreign origin still gets 204 (handled by route)
	// but without the allow header.
	resp = doRequest(t, http.MethodOptions, base+"/api/v1/overview", map[string]string{"Origin": "https://evil.example"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight -> %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("foreign preflight got allow-origin %q", got)
	}
}

// TestDashboardMethods: the dashboard answers GET and HEAD and rejects POST.
func TestDashboardMethods(t *testing.T) {
	_, base := startServerOn(t, Config{Token: "t", Dashboard: true})

	resp := doRequest(t, http.MethodPost, base+"/", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST / -> %d, want 405", resp.StatusCode)
	}
	resp = doRequest(t, http.MethodHead, base+"/dashboard", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD /dashboard -> %d, want 200", resp.StatusCode)
	}

	// Without Dashboard the root is a JSON 404, not the dashboard page.
	_, base2 := startServerOn(t, Config{Token: "t"})
	resp = doRequest(t, http.MethodGet, base2+"/", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("root with dashboard off -> %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("404 content type = %q, want json", ct)
	}
}

// TestStopIdempotent: Stop before Start and a repeated Stop are both no-ops,
// and Addr reports nil before a listener exists.
func TestStopIdempotent(t *testing.T) {
	c := stats.New(stats.Config{})
	s, err := New(Config{ListenAddr: "127.0.0.1:0", Token: "t"}, Deps{Stats: c})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if s.Addr() != nil {
		t.Fatal("Addr before Start must be nil")
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("stop before start: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if s.Addr() == nil {
		t.Fatal("Addr after Start must be set")
	}
	// Starting twice is a no-op, not an error.
	if err := s.Start(); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}

// TestStartBindFailure: a Start that cannot bind reports the error instead of
// serving on nothing.
func TestStartBindFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	c := stats.New(stats.Config{})
	s, err := New(Config{ListenAddr: occupied.Addr().String(), Token: "t"}, Deps{Stats: c})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := s.Start(); err == nil {
		t.Fatal("start on an occupied port must fail")
	}
	_ = s.Stop()
}

// TestStatusWithoutInfoProvider: with no Info provider the version falls back
// to Deps.Version and no node merge happens.
func TestStatusWithoutInfoProvider(t *testing.T) {
	c := stats.New(stats.Config{HistoryMinutes: 60, BucketInterval: time.Minute})
	srv, err := New(Config{ListenAddr: "127.0.0.1:0", Token: "t"}, Deps{Stats: c, Version: "fallback-2.0"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	base := "http://" + srv.Addr().String()

	var out StatusResponse
	getJSON(t, base+"/api/v1/status", "t", &out)
	if out.Version != "fallback-2.0" || len(out.Nodes) != 0 {
		t.Fatalf("status without Info = %+v", out.SystemInfo)
	}

	// An empty-stats snapshot takes the no-ratio path.
	var snap SnapshotResponse
	getJSON(t, base+"/api/v1/snapshot", "t", &snap)
	if snap.Overview.Totals.Conns != 0 || snap.Routes.DirectConnRatio != 0 {
		t.Fatalf("empty snapshot ratios = %+v", snap.Routes)
	}
	// Routes on empty stats: zero conns means zero ratios, no division.
	var routes RoutesResponse
	getJSON(t, base+"/api/v1/routes", "t", &routes)
	if routes.Totals.Conns != 0 || routes.DirectConnRatio != 0 || routes.ProxyBytesRatio != 0 {
		t.Fatalf("empty routes = %+v", routes)
	}
}

// TestTrafficMaxPointsTruncation: a wide range at a small step is truncated to
// MaxPoints instead of flooding the client.
func TestTrafficMaxPointsTruncation(t *testing.T) {
	_, base := startServerOn(t, Config{Token: "t"})
	var out TrafficResponse
	getJSON(t, base+"/api/v1/traffic?range=7d&step=1m", "t", &out)
	if len(out.Points) > MaxPoints {
		t.Fatalf("points = %d, must be capped at %d", len(out.Points), MaxPoints)
	}
	if out.Range != "604800s" {
		t.Fatalf("range label = %q", out.Range)
	}

	// Snapshot with the same wide window stays bounded too.
	var snap SnapshotResponse
	getJSON(t, base+"/api/v1/snapshot?range=7d&step=1m", "t", &snap)
	if len(snap.Traffic.Points) > MaxPoints {
		t.Fatalf("snapshot points = %d", len(snap.Traffic.Points))
	}
}

// TestParseCIDRIPv6Bare: a bare IPv6 address is treated as a /128 host.
func TestParseCIDRIPv6Bare(t *testing.T) {
	c := stats.New(stats.Config{})
	srv, err := New(Config{Token: "t", AllowedCIDRs: []string{"::1"}}, Deps{Stats: c})
	if err != nil {
		t.Fatalf("bare ipv6 address: %v", err)
	}
	if len(srv.allowedNets) != 1 {
		t.Fatalf("allowedNets = %d, want 1", len(srv.allowedNets))
	}
	// Blank entries are skipped silently.
	srv2, err := New(Config{Token: "t", AllowedCIDRs: []string{"", "   ", "127.0.0.1/32"}}, Deps{Stats: c})
	if err != nil {
		t.Fatalf("blank entries: %v", err)
	}
	if len(srv2.allowedNets) != 1 {
		t.Fatalf("blank entries produced %d nets, want 1", len(srv2.allowedNets))
	}
	// A malformed client list fails construction with its own prefix.
	if _, err := New(Config{Token: "t", ClientCIDRs: []string{"nope"}}, Deps{Stats: c}); err == nil {
		t.Fatal("invalid client CIDR should fail at construction")
	}
}

// TestHealthHEADAndMethodRejection: /api/v1/health answers HEAD, and a wrong
// method on a read endpoint carries the Allow header.
func TestHealthHEADAndMethodRejection(t *testing.T) {
	_, base := startServerOn(t, Config{Token: "t"})

	resp := doRequest(t, http.MethodHead, base+"/api/v1/health", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD /health -> %d, want 200", resp.StatusCode)
	}

	resp = doRequest(t, http.MethodPost, base+"/api/v1/overview", map[string]string{"Authorization": "Bearer t"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /overview -> %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") {
		t.Fatalf("Allow header = %q", allow)
	}
}

// TestJSONErrorBodyShape: the 401 envelope carries the documented fields.
func TestJSONErrorBodyShape(t *testing.T) {
	_, base := startServerOn(t, Config{Token: "t"})
	status, body := get(t, base+"/api/v1/overview", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("no token -> %d", status)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("bad json: %v (%s)", err, body)
	}
	if _, ok := out["error"]; !ok {
		t.Fatalf("missing error field: %s", body)
	}
	if _, ok := out["now"]; !ok {
		t.Fatalf("missing now field: %s", body)
	}
}

// emptyVersionInfo reports a SystemInfo with no version so the status handler
// must fall back to Deps.Version.
type emptyVersionInfo struct{}

func (emptyVersionInfo) APIInfo() SystemInfo {
	return SystemInfo{Running: true, Nodes: []NodeInfo{{Name: "node-a"}}}
}

func TestStatusFallsBackToDepsVersion(t *testing.T) {
	c := stats.New(stats.Config{})
	srv, err := New(Config{ListenAddr: "127.0.0.1:0", Token: "t"},
		Deps{Stats: c, Info: emptyVersionInfo{}, Version: "fallback-3.0"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	base := "http://" + srv.Addr().String()

	var out StatusResponse
	getJSON(t, base+"/api/v1/status", "t", &out)
	if out.Version != "fallback-3.0" {
		t.Fatalf("status version = %q, want the Deps.Version fallback", out.Version)
	}
	if !out.Running || len(out.Nodes) != 1 {
		t.Fatalf("status = %+v", out.SystemInfo)
	}

	var nodes NodesResponse
	getJSON(t, base+"/api/v1/nodes", "t", &nodes)
	if nodes.Count != 1 || nodes.Nodes[0].Name != "node-a" {
		t.Fatalf("nodes = %+v", nodes)
	}
}

// TestNewReaderBodyIsDrained makes sure the error paths still return valid
// JSON bodies that can be fully read (guards against missing WriteHeader).
func TestErrorBodiesAreCompleteJSON(t *testing.T) {
	_, base := startServerOn(t, Config{Token: "t", AllowedCIDRs: []string{"10.9.9.9/32"}})
	resp := doRequest(t, http.MethodGet, base+"/api/v1/overview", map[string]string{"Authorization": "Bearer t"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign peer -> %d", resp.StatusCode)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("403 body is not json: %v (%s)", err, body)
	}
}
