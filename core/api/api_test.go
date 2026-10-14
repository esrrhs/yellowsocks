package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/esrrhs/yellowsocks/core/stats"
)

type fakeInfo struct{}

func (fakeInfo) APIInfo() SystemInfo {
	return SystemInfo{
		Version: "test-1.0",
		Running: true,
		Listeners: Listeners{
			DNS:    "127.0.0.1:53",
			Socks5: "127.0.0.1:1080",
			HTTP:   "127.0.0.1:8080",
			API:    "127.0.0.1:9090",
		},
		Router: RouterInfo{DirectDomains: 12, ProxyDomains: 3, GeoIP: true, SkipCountry: "CN"},
		Nodes: []NodeInfo{
			{Name: "node-a", Server: "1.2.3.4:8888", Proto: "tcp", Alive: true, Active: true},
			{Name: "node-b", Server: "5.6.7.8:8888", Proto: "quic", Alive: false},
		},
	}
}

// newTestServer starts an API server on an ephemeral port with seeded data.
func newTestServer(t *testing.T, token string) (*Server, string) {
	t.Helper()
	c := stats.New(stats.Config{HistoryMinutes: 60, BucketInterval: time.Minute})
	s := c.Begin(stats.ProtoHTTP, "10.0.0.1:1234", "example.com", 443, stats.DecisionProxy, "node-a")
	s.AddUpload(2048)
	s.AddDownload(8192)
	s.End(nil)
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
	t.Cleanup(func() { _ = srv.Stop() })
	addr := srv.Addr().String()
	return srv, "http://" + addr
}

func get(t *testing.T, url, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func getJSON(t *testing.T, url, token string, out interface{}) int {
	t.Helper()
	status, body := get(t, url, token)
	if status != http.StatusOK {
		t.Fatalf("%s -> %d: %s", url, status, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("%s -> bad json: %v (%s)", url, err, body)
	}
	return status
}

func TestAuthRequired(t *testing.T) {
	_, base := newTestServer(t, "secret")

	if status, body := get(t, base+"/api/v1/overview", ""); status != http.StatusUnauthorized {
		t.Fatalf("no token -> %d, want 401 (%s)", status, body)
	}
	if status, _ := get(t, base+"/api/v1/overview", "wrong"); status != http.StatusUnauthorized {
		t.Fatalf("wrong token -> %d, want 401", status)
	}
	if status, body := get(t, base+"/api/v1/overview", "secret"); status != http.StatusOK {
		t.Fatalf("bearer token -> %d (%s)", status, body)
	}
	// X-API-Token header.
	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/overview", nil)
	req.Header.Set("X-API-Token", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("X-API-Token -> %d, want 200", resp.StatusCode)
	}
	// Query parameter.
	if status, body := get(t, base+"/api/v1/overview?token=secret", ""); status != http.StatusOK {
		t.Fatalf("query token -> %d (%s)", status, body)
	}
	if status, _ := get(t, base+"/api/v1/overview?token=nope", ""); status != http.StatusUnauthorized {
		t.Fatalf("bad query token -> %d, want 401", status)
	}
}

func TestHealthIsPublic(t *testing.T) {
	_, base := newTestServer(t, "secret")
	var out map[string]interface{}
	getJSON(t, base+"/api/v1/health", "", &out)
	if out["status"] != "ok" {
		t.Fatalf("health = %+v", out)
	}
}

func TestUnknownEndpoint(t *testing.T) {
	_, base := newTestServer(t, "secret")
	if status, _ := get(t, base+"/api/v1/nope", "secret"); status != http.StatusNotFound {
		t.Fatalf("unknown endpoint -> %d, want 404", status)
	}
}

func TestOverview(t *testing.T) {
	_, base := newTestServer(t, "secret")
	var out OverviewResponse
	getJSON(t, base+"/api/v1/overview", "secret", &out)
	if out.Totals.Upload != 2048 || out.Totals.Download != 8192 || out.Totals.Conns != 1 {
		t.Fatalf("overview totals = %+v", out.Totals)
	}
	if out.Proxy.Conns != 1 || out.Direct.Conns != 0 {
		t.Fatalf("overview split = direct %+v proxy %+v", out.Direct, out.Proxy)
	}
	if out.Version != "test-1.0" {
		t.Fatalf("version = %q", out.Version)
	}
}

func TestStatus(t *testing.T) {
	_, base := newTestServer(t, "secret")
	var out StatusResponse
	getJSON(t, base+"/api/v1/status", "secret", &out)
	if !out.Running || out.Version != "test-1.0" {
		t.Fatalf("status = %+v", out)
	}
	if out.Listeners.Socks5 != "127.0.0.1:1080" || out.Listeners.API != "127.0.0.1:9090" {
		t.Fatalf("listeners = %+v", out.Listeners)
	}
	if out.Router.DirectDomains != 12 || !out.Router.GeoIP {
		t.Fatalf("router = %+v", out.Router)
	}
	if len(out.Nodes) != 2 {
		t.Fatalf("nodes = %+v", out.Nodes)
	}
	// node-a carried the seeded session, so its traffic is merged in.
	var found bool
	for _, n := range out.Nodes {
		if n.Name == "node-a" && n.Conns == 1 && n.Upload == 2048 {
			found = true
		}
	}
	if !found {
		t.Fatalf("node traffic not merged: %+v", out.Nodes)
	}
	if out.History.BucketSeconds != 60 || out.History.RetentionBucket != 60 {
		t.Fatalf("history = %+v", out.History)
	}
}

func TestConnectionsDomainsRoutesDNSErrors(t *testing.T) {
	_, base := newTestServer(t, "secret")

	var dns stats.DNSStat
	getJSON(t, base+"/api/v1/dns?limit=5", "secret", &dns)
	if dns.Queries != 1 || dns.DoH != 1 {
		t.Fatalf("dns = %+v", dns)
	}

	var routes RoutesResponse
	getJSON(t, base+"/api/v1/routes?limit=5", "secret", &routes)
	if routes.Proxy.Conns != 1 || routes.ProxyConnRatio != 1 {
		t.Fatalf("routes = %+v", routes)
	}
	if len(routes.TopProxyDomains) != 1 || routes.TopProxyDomains[0].Domain != "example.com" {
		t.Fatalf("top proxy domains = %+v", routes.TopProxyDomains)
	}

	var errs ErrorsResponse
	getJSON(t, base+"/api/v1/errors?limit=10", "secret", &errs)
	if errs.Count != 1 || errs.Errors[0].Scope != "http" || errs.Errors[0].Host != "bad.com" {
		t.Fatalf("errors = %+v", errs)
	}
	// Scope filter drops everything else.
	var filtered ErrorsResponse
	getJSON(t, base+"/api/v1/errors?scope=dns", "secret", &filtered)
	if filtered.Count != 0 {
		t.Fatalf("scope filter = %+v", filtered)
	}

	var nodes NodesResponse
	getJSON(t, base+"/api/v1/nodes", "secret", &nodes)
	if nodes.Count != 2 {
		t.Fatalf("nodes = %+v", nodes)
	}
}

func TestTrafficSeries(t *testing.T) {
	_, base := newTestServer(t, "secret")
	var out TrafficResponse
	getJSON(t, base+"/api/v1/traffic?range=1h", "secret", &out)
	if out.StepSecond != 60 {
		t.Fatalf("step = %d, want 60", out.StepSecond)
	}
	if len(out.Points) != 61 {
		t.Fatalf("points = %d, want 61 for 1h at 1m", len(out.Points))
	}
	var up, down int64
	for _, p := range out.Points {
		up += p.Upload
		down += p.Download
	}
	if up != 2048 || down != 8192 {
		t.Fatalf("series sums up=%d down=%d, want 2048/8192", up, down)
	}

	// An explicit step wins.
	var fiveMin TrafficResponse
	getJSON(t, base+"/api/v1/traffic?range=6h&step=5m", "secret", &fiveMin)
	if fiveMin.StepSecond != 300 {
		t.Fatalf("step = %d, want 300", fiveMin.StepSecond)
	}
	// A bogus range falls back to the default instead of failing.
	var bogus TrafficResponse
	getJSON(t, base+"/api/v1/traffic?range=notaduration", "secret", &bogus)
	if len(bogus.Points) == 0 {
		t.Fatal("bogus range produced no points")
	}
}

func TestSnapshotAndReset(t *testing.T) {
	srv, base := newTestServer(t, "secret")
	var snap SnapshotResponse
	getJSON(t, base+"/api/v1/snapshot", "secret", &snap)
	if snap.Overview.Totals.Conns != 1 || len(snap.Domains) == 0 || snap.Status.Version != "test-1.0" {
		t.Fatalf("snapshot = %+v", snap)
	}

	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/reset", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset -> %d", resp.StatusCode)
	}
	if got := srv.deps.Stats.Overview().Totals.Conns; got != 0 {
		t.Fatalf("after reset conns = %d, want 0", got)
	}

	// GET is not allowed on the reset route.
	if status, _ := get(t, base+"/api/v1/reset", "secret"); status == http.StatusOK {
		t.Fatal("GET /reset should not succeed")
	}
}

func TestDashboardServed(t *testing.T) {
	_, base := newTestServer(t, "secret")
	status, body := get(t, base+"/", "")
	if status != http.StatusOK {
		t.Fatalf("dashboard -> %d", status)
	}
	if len(body) == 0 || string(body[:15]) != "<!DOCTYPE html>" {
		t.Fatalf("dashboard body looks wrong: %.20q", body)
	}
	// The page must let the operator point it at another host.
	if !strings.Contains(string(body), `id="server"`) || !strings.Contains(string(body), `id="token"`) {
		t.Fatal("dashboard is missing the server/token inputs")
	}
	if !strings.Contains(string(body), "ys_server") {
		t.Fatal("dashboard does not persist the server address")
	}
}

func TestCORS(t *testing.T) {
	_, base := newTestServer(t, "secret")

	req, _ := http.NewRequest(http.MethodOptions, base+"/api/v1/overview", nil)
	req.Header.Set("Origin", "https://example.org")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight -> %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("allow-origin = %q", got)
	}

	req2, _ := http.NewRequest(http.MethodGet, base+"/api/v1/overview?token=secret", nil)
	req2.Header.Set("Origin", "https://example.org")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp2.Body.Close()
	if got := resp2.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("allow-origin on GET = %q", got)
	}
}

func TestTokenGeneratedWhenEmpty(t *testing.T) {
	c := stats.New(stats.Config{})
	srv, err := New(Config{ListenAddr: "127.0.0.1:0"}, Deps{Stats: c, Info: fakeInfo{}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if srv.Token() == "" {
		t.Fatal("empty token should be replaced by a generated one")
	}
}

// request is a small helper for requests with arbitrary headers.
func doRequest(t *testing.T, method, url string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// startServerOn builds a server with the given config on an ephemeral port.
func startServerOn(t *testing.T, cfg Config) (*Server, string) {
	t.Helper()
	c := stats.New(stats.Config{HistoryMinutes: 60, BucketInterval: time.Minute})
	cfg.ListenAddr = "127.0.0.1:0"
	if cfg.Token == "" {
		cfg.Token = "secret"
	}
	srv, err := New(cfg, Deps{Stats: c, Info: fakeInfo{}, Version: "test-1.0"})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	return srv, "http://" + srv.Addr().String()
}

func TestReadonlyTokenCannotMutate(t *testing.T) {
	_, base := startServerOn(t, Config{Token: "admin-token", ReadonlyToken: "ro-token"})

	// Read-only token reads fine.
	resp := doRequest(t, http.MethodGet, base+"/api/v1/overview", map[string]string{"Authorization": "Bearer ro-token"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readonly GET -> %d, want 200", resp.StatusCode)
	}
	// ...but cannot reset: 403, not 401, because the token itself is valid.
	resp = doRequest(t, http.MethodPost, base+"/api/v1/reset", map[string]string{"Authorization": "Bearer ro-token"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("readonly POST reset -> %d, want 403", resp.StatusCode)
	}
	// The admin token can.
	resp = doRequest(t, http.MethodPost, base+"/api/v1/reset", map[string]string{"Authorization": "Bearer admin-token"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin POST reset -> %d, want 200", resp.StatusCode)
	}
	// An unknown token gets 401 even on a read endpoint.
	resp = doRequest(t, http.MethodGet, base+"/api/v1/overview", map[string]string{"Authorization": "Bearer nope"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token -> %d, want 401", resp.StatusCode)
	}
}

func TestClientCIDRWhitelist(t *testing.T) {
	// Only the loopback host may call: enough to prove the check runs.
	_, base := startServerOn(t, Config{Token: "t", AllowedCIDRs: []string{"127.0.0.1/32"}})
	resp := doRequest(t, http.MethodGet, base+"/api/v1/overview", map[string]string{"Authorization": "Bearer t"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("allowed client -> %d, want 200", resp.StatusCode)
	}

	// A whitelist the client is not on blocks it, even with a good token.
	_, base2 := startServerOn(t, Config{Token: "t", AllowedCIDRs: []string{"10.0.0.1/32"}})
	resp = doRequest(t, http.MethodGet, base2+"/api/v1/overview", map[string]string{"Authorization": "Bearer t"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign client -> %d, want 403", resp.StatusCode)
	}
	// Health is public but still network-gated.
	resp = doRequest(t, http.MethodGet, base2+"/api/v1/health", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("health from foreign client -> %d, want 403", resp.StatusCode)
	}
}

// A peer whitelist must keep working no matter what forwarding headers the
// peer sends: it gates on who connected, not on what the headers claim.
func TestPeerWhitelistIgnoresForwardedHeaders(t *testing.T) {
	// Only this host may connect; it is also declared as a trusted proxy,
	// which is exactly how a reverse proxy is configured.
	_, base := startServerOn(t, Config{
		Token:          "t",
		AllowedCIDRs:   []string{"127.0.0.1/32"},
		TrustedProxies: []string{"127.0.0.1/32"},
	})
	// No header, and a header naming the browser behind the proxy: both
	// must pass, because the peer is the proxy we allow.
	for _, xff := range []string{"", "203.0.113.5"} {
		headers := map[string]string{"Authorization": "Bearer t"}
		if xff != "" {
			headers["X-Forwarded-For"] = xff
		}
		resp := doRequest(t, http.MethodGet, base+"/api/v1/overview", headers)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("peer whitelist with XFF %q -> %d, want 200", xff, resp.StatusCode)
		}
	}

	// A peer outside the whitelist is refused even when its headers claim
	// an allowed address.
	_, base2 := startServerOn(t, Config{Token: "t", AllowedCIDRs: []string{"10.0.0.1/32"}})
	resp := doRequest(t, http.MethodGet, base2+"/api/v1/overview", map[string]string{
		"Authorization": "Bearer t", "X-Forwarded-For": "10.0.0.1",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("forged XFF -> %d, want 403", resp.StatusCode)
	}
}

// Filtering on the forwarded client is opt-in and only works through a
// trusted proxy, so an untrusted client cannot forge its way in.
func TestClientWhitelistNeedsTrustedProxy(t *testing.T) {
	cfg := Config{Token: "t", ClientCIDRs: []string{"10.0.0.1/32"}}

	// Without a trusted proxy the header is ignored: the real peer (us) is
	// not on the client list, so the call is refused.
	_, base := startServerOn(t, cfg)
	resp := doRequest(t, http.MethodGet, base+"/api/v1/overview", map[string]string{
		"Authorization": "Bearer t", "X-Forwarded-For": "10.0.0.1",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("untrusted XFF -> %d, want 403", resp.StatusCode)
	}

	// With the peer trusted, the forwarded address is used: allowed ...
	cfg.TrustedProxies = []string{"127.0.0.1/32"}
	_, base2 := startServerOn(t, cfg)
	resp = doRequest(t, http.MethodGet, base2+"/api/v1/overview", map[string]string{
		"Authorization": "Bearer t", "X-Forwarded-For": "10.0.0.1",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("trusted proxy XFF -> %d, want 200", resp.StatusCode)
	}
	// ... and a visitor outside the list is refused. The last untrusted hop
	// in a chain is the one that counts.
	resp = doRequest(t, http.MethodGet, base2+"/api/v1/overview", map[string]string{
		"Authorization": "Bearer t", "X-Forwarded-For": "203.0.113.5, 127.0.0.1",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("untrusted hop in chain -> %d, want 403", resp.StatusCode)
	}
}

func TestCORSDisabled(t *testing.T) {
	_, base := startServerOn(t, Config{Token: "t", AllowedOrigins: []string{"none"}})
	resp := doRequest(t, http.MethodGet, base+"/api/v1/overview?token=t", map[string]string{"Origin": "https://elsewhere.example"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("same-origin style call -> %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("CORS disabled but got Allow-Origin %q", got)
	}
}

func TestInvalidCIDRConfig(t *testing.T) {
	c := stats.New(stats.Config{})
	if _, err := New(Config{Token: "t", AllowedCIDRs: []string{"not-a-cidr"}}, Deps{Stats: c}); err == nil {
		t.Fatal("invalid CIDR should fail at construction")
	}
	if _, err := New(Config{Token: "t", TrustedProxies: []string{"10.0.0.0/33"}}, Deps{Stats: c}); err == nil {
		t.Fatal("invalid trusted proxy CIDR should fail at construction")
	}
	// A bare address is accepted and treated as a single host.
	srv, err := New(Config{Token: "t", AllowedCIDRs: []string{"127.0.0.1"}}, Deps{Stats: c})
	if err != nil {
		t.Fatalf("bare address: %v", err)
	}
	if len(srv.allowedNets) != 1 {
		t.Fatalf("allowedNets = %d, want 1", len(srv.allowedNets))
	}
}

func TestParseDuration(t *testing.T) {
	cases := map[string]time.Duration{
		"30m": 30 * time.Minute,
		"6h":  6 * time.Hour,
		"7d":  7 * 24 * time.Hour,
		"90s": 90 * time.Second,
		"120": 120 * time.Second,
	}
	for in, want := range cases {
		got, err := parseDuration(in)
		if err != nil || got != want {
			t.Fatalf("parseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseDuration("abc"); err == nil {
		t.Fatal("parseDuration should reject garbage")
	}
}
