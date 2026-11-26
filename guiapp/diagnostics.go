package guiapp

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Connectivity checks. These deliberately probe the real path instead of the
// components: "domestic" dials straight out (so it exercises TUN capture and
// the direct rule), "international" goes through the local HTTP proxy (so it
// exercises rule matching, the upstream SOCKS5 hop and TLS in one request).
//
// Everything runs in parallel with its own short timeout: a user clicking
// "测速" should get an answer in seconds, including when one leg is dead.

const (
	probeTimeout = 8 * time.Second
	dialTimeout  = 5 * time.Second

	// The exit-IP lookup falls through several providers (see geoSources).
	// Each leg is quick and short-lived, and the chain has an overall budget,
	// so a rate-limited provider costs one leg instead of the whole report.
	geoLegTimeout = 5 * time.Second
	geoBudget     = 9 * time.Second
)

// ProbeItem is one line of the report.
//
// The report carries no display text: the row's label is looked up from Key
// ("direct", "proxied", ...) in the frontend catalogue, and DetailKey covers
// the outcomes the app itself words ("no upstream configured", ...). Detail
// stays raw -- a Go error string or an HTTP status -- because it is diagnostic
// output, not a sentence we can translate meaningfully.
type ProbeItem struct {
	Key       string `json:"key"`
	Target    string `json:"target"`
	OK        bool   `json:"ok"`
	Ms        int64  `json:"ms"`
	Status    int    `json:"status"`
	DetailKey string `json:"detail_key,omitempty"`
	Detail    string `json:"detail"`
}

// ProbeReport is what the UI renders: the individual legs plus the exit
// address, which is the only way to tell "the proxy works" from "the proxy
// works but egresses from somewhere useless".
//
// The location is reported as an ISO 3166-1 alpha-2 code and the provider's
// own city string, never as a sentence: the window turns the code into a
// country name in the user's language (Intl.DisplayNames), and a city name has
// no translation in either catalogue.
type ProbeReport struct {
	At          int64       `json:"at"`
	ExitIP      string      `json:"exit_ip"`
	ExitCountry string      `json:"exit_country"`
	ExitCity    string      `json:"exit_city"`
	Items       []ProbeItem `json:"items"`
}

// TestConnectivity runs the probe set against the current configuration.
func (a *App) TestConnectivity() ProbeReport {
	a.mu.Lock()
	httpAddr := a.lastCfg.HTTPListen
	upstream := a.lastCfg.SPPServer
	running := a.running
	a.mu.Unlock()

	report := ProbeReport{At: time.Now().Unix()}
	if !running {
		report.Items = []ProbeItem{{
			Key: "engine", OK: false, DetailKey: "engine_stopped",
		}}
		return report
	}

	items := make([]ProbeItem, 4)
	var wg sync.WaitGroup
	wg.Add(4)

	go func() {
		defer wg.Done()
		items[0] = probeHTTPGet("direct", "https://www.baidu.com", "", nil)
	}()
	go func() {
		defer wg.Done()
		items[1] = probeHTTPGet("proxied", "https://www.google.com", httpAddr, nil)
	}()
	go func() {
		defer wg.Done()
		items[2] = probeUpstream(upstream)
	}()
	go func() {
		defer wg.Done()
		item, geo := probeExit(httpAddr)
		items[3] = item
		report.ExitIP = geo.ip
		report.ExitCountry = geo.country
		report.ExitCity = geo.city
	}()

	wg.Wait()
	report.Items = items
	return report
}

// probeHTTPGet performs one GET and reports status and elapsed time. proxyAddr is
// the local HTTP proxy ("host:port"), empty for a direct request. When body is
// non-nil up to 4KB of the response is stored in it.
func probeHTTPGet(key, rawURL, proxyAddr string, body *string) ProbeItem {
	return probeHTTPGetWithin(key, rawURL, proxyAddr, body, probeTimeout)
}

func probeHTTPGetWithin(key, rawURL, proxyAddr string, body *string, timeout time.Duration) ProbeItem {
	item := ProbeItem{Key: key, Target: rawURL}
	tr := &http.Transport{
		// No keep-alives: a reused connection would report the handshake of a
		// previous probe instead of this one.
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: timeout,
		DialContext:         (&net.Dialer{Timeout: dialTimeout}).DialContext,
	}
	if proxyAddr != "" {
		if pu, err := url.Parse("http://" + proxyAddr); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	client := &http.Client{Transport: tr, Timeout: timeout}

	start := time.Now()
	resp, err := client.Get(rawURL)
	item.Ms = time.Since(start).Milliseconds()
	if err != nil {
		item.Detail = shortenErr(err)
		return item
	}
	defer resp.Body.Close()
	item.Status = resp.StatusCode
	item.OK = resp.StatusCode > 0 && resp.StatusCode < 500
	item.Detail = resp.Status
	if body != nil {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		*body = string(buf)
	}
	return item
}

// exitGeo is what an exit lookup managed to learn. Any field may be empty:
// the address alone is still worth showing, and the country is what the extra
// lookups are for.
type exitGeo struct {
	ip      string
	country string // ISO 3166-1 alpha-2, upper case
	city    string // provider's own spelling, not translated
}

// geoSources are tried in order. Each one fails differently: ipinfo is the
// richest but rate-limits by egress IP (a shared proxy exit answers 429),
// ipwho.is is generous and key-free, Cloudflare answers from every edge but
// only knows the country, and ipify only knows the address -- which is still
// exactly what the row showed before location was added, so the worst case is
// the old behaviour rather than a failure.
var geoSources = []struct {
	url   string
	parse func(string) exitGeo
}{
	{"https://ipinfo.io/json", parseIPInfo},
	{"https://ipwho.is/", parseIPWho},
	{"https://www.cloudflare.com/cdn-cgi/trace", parseCloudflareTrace},
	{"https://api.ipify.org?format=json", parseIPOnly},
}

// probeExit resolves the address the traffic actually leaves from, plus where
// that address is registered.
//
// The legs run one after another, so the wait is bounded by geoBudget: the
// report is only as fast as its slowest row, and one dead provider must not
// stall the other three.
func probeExit(proxyAddr string) (ProbeItem, exitGeo) {
	deadline := time.Now().Add(geoBudget)
	var (
		item ProbeItem
		geo  exitGeo
		got  bool // an address has been resolved
	)
	for i, src := range geoSources {
		if i > 0 && time.Now().After(deadline) {
			break
		}
		body := ""
		leg := probeHTTPGetWithin("exitip", src.url, proxyAddr, &body, geoLegTimeout)
		if leg.OK {
			if g := src.parse(body); g.ip != "" {
				g.city = strings.TrimSpace(g.city)
				geo, got = g, true
				leg.Detail = g.ip
				item = leg
				// A provider that also knows the country is the last one we
				// need; an address-only answer keeps the chain going.
				if g.country != "" {
					break
				}
				continue
			}
			// 2xx but nothing we understand: a captive portal or an error
			// page served with a 200. Treat it as a failed leg.
			leg.OK = false
			leg.Detail = ""
			leg.DetailKey = "unparsed"
		}
		if !got {
			item = leg
		}
	}
	if item.Key == "" {
		// Unreachable in practice (the table is non-empty), but the row must
		// never render blank.
		item = ProbeItem{Key: "exitip"}
	}
	return item, geo
}

// normCountry keeps only codes worth showing: two letters, upper case. "XX"
// (unknown) and "T1" (Tor) come back from public lookups and would render as
// a nonexistent country name.
func normCountry(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) != 2 || s[0] < 'A' || s[0] > 'Z' || s[1] < 'A' || s[1] > 'Z' {
		return ""
	}
	if s == "XX" || s == "T1" {
		return ""
	}
	return s
}

func parseIPInfo(body string) exitGeo {
	var v struct {
		IP      string `json:"ip"`
		Country string `json:"country"`
		City    string `json:"city"`
	}
	if json.Unmarshal([]byte(body), &v) != nil {
		return exitGeo{}
	}
	return exitGeo{ip: v.IP, country: normCountry(v.Country), city: v.City}
}

func parseIPWho(body string) exitGeo {
	var v struct {
		Success     bool   `json:"success"`
		IP          string `json:"ip"`
		CountryCode string `json:"country_code"`
		City        string `json:"city"`
	}
	if json.Unmarshal([]byte(body), &v) != nil || !v.Success {
		return exitGeo{}
	}
	return exitGeo{ip: v.IP, country: normCountry(v.CountryCode), city: v.City}
}

// parseCloudflareTrace reads the plain-text key=value dump of /cdn-cgi/trace.
func parseCloudflareTrace(body string) exitGeo {
	var g exitGeo
	for _, line := range strings.Split(body, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "ip":
			g.ip = v
		case "loc":
			g.country = normCountry(v)
		}
	}
	return g
}

func parseIPOnly(body string) exitGeo {
	return exitGeo{ip: parseIPify(body)}
}

// probeUpstream checks that the configured upstream SOCKS5 port accepts a TCP
// connection. A refused connection here explains every other failure, and it
// is the fastest way to tell "upstream is down" from "rules are wrong".
func probeUpstream(addr string) ProbeItem {
	item := ProbeItem{Key: "upstream", Target: addr}
	if strings.TrimSpace(addr) == "" {
		item.DetailKey = "not_configured"
		return item
	}
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	item.Ms = time.Since(start).Milliseconds()
	if err != nil {
		item.Detail = shortenErr(err)
		return item
	}
	_ = conn.Close()
	item.OK = true
	item.DetailKey = "tcp_ok"
	return item
}

func parseIPify(body string) string {
	var v struct {
		IP string `json:"ip"`
	}
	if err := json.Unmarshal([]byte(body), &v); err == nil && v.IP != "" {
		return v.IP
	}
	// Plain-text fallback (ifconfig.me style), one line.
	if s := strings.TrimSpace(body); s != "" && len(s) <= 64 && !strings.ContainsAny(s, "{}\"") {
		return s
	}
	return ""
}

// shortenErr keeps the report readable: Go nests the same address in three
// error wrappers, and the UI has one line per row.
func shortenErr(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, ": "); i > 0 && len(msg) > 120 {
		msg = msg[i+2:]
	}
	if len(msg) > 160 {
		msg = msg[:160] + "…"
	}
	return msg
}
