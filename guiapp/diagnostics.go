package guiapp

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
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

	// The automatic self-check fires at this cadence while the engine runs,
	// and once shortly after every engine start (see probeLoop). One minute
	// is slow enough not to matter on any bill, fast enough that the strip
	// on the home page is never quoting a dead link for long.
	probeInterval   = time.Minute
	probeStartDelay = 3 * time.Second
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
	// The direct path's egress: what the machine looks like to domestic
	// sites. Shown next to the proxied exit, because "which address does
	// the world see" has two honest answers on a split-tunnel setup.
	ExitIPDirect      string      `json:"exit_ip_direct,omitempty"`
	ExitCountryDirect string      `json:"exit_country_direct,omitempty"`
	ExitCityDirect    string      `json:"exit_city_direct,omitempty"`
	Items             []ProbeItem `json:"items"`
}

// runProbe executes the probe set once, caches the report and pushes it to
// the window. Single-flight: the minute timer, the engine-start kick and a
// manual click can all fire together, and a second concurrent set would
// double the outbound traffic without adding information — the racing
// caller is answered with the cached report instead.
func (a *App) runProbe() ProbeReport {
	if !a.probing.CompareAndSwap(false, true) {
		if last := a.lastProbe.Load(); last != nil {
			return *last
		}
		return ProbeReport{At: time.Now().Unix(), Items: []ProbeItem{}}
	}
	defer a.probing.Store(false)
	r := a.probeOnce()
	a.lastProbe.Store(&r)
	a.emitProbe(r)
	return r
}

// TestConnectivity runs the probe set against the current configuration.
// The automatic loop in probeLoop calls the same path.
func (a *App) TestConnectivity() ProbeReport {
	return a.runProbe()
}

// GetLastProbe returns the newest self-check report, or nil before the first
// one has run. The window reads it once at boot so the strip shows something
// without waiting for the next tick.
func (a *App) GetLastProbe() *ProbeReport {
	return a.lastProbe.Load()
}

// probeLoop runs the automatic self-check for the life of the process: once
// per minute while the engine runs, and once shortly after every engine
// start, so a freshly established tunnel reports while the user is still
// looking at the home page.
//
// The timer lives in the backend on purpose: WebView2 throttles window
// timers while the window sits hidden in the tray, and a self-check that
// stalls whenever the window is hidden would go stale exactly when nobody
// is watching to notice.
func (a *App) probeLoop() {
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.shutdownDone:
			return
		case <-a.probeKick:
			// The listeners are up, but the first seconds can still see
			// route churn (TUN bring-up runs in parallel with the engine
			// start); let it settle, then report. An engine that went down
			// again in the meantime skips the round.
			select {
			case <-time.After(probeStartDelay):
			case <-a.shutdownDone:
				return
			}
			if a.engineRunning() {
				a.runProbe()
			}
		case <-ticker.C:
			if a.engineRunning() {
				a.runProbe()
			}
		}
	}
}

func (a *App) engineRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// emitProbe pushes the report to the window, gated like the status event:
// tests and driver builds have no Wails context, and the runtime exits the
// process if it is used without one.
func (a *App) emitProbe(r ProbeReport) {
	a.mu.Lock()
	on := a.frontendEvents
	a.mu.Unlock()
	if !on {
		return
	}
	runtime.EventsEmit(a.ctx, "probe:report", r)
}

// probeOnce runs the probe set against the current configuration.
func (a *App) probeOnce() ProbeReport {
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

	items := make([]ProbeItem, 5)
	var wg sync.WaitGroup
	wg.Add(5)

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
		item, geo := probeExit(httpAddr, "exitip", geoSources)
		items[3] = item
		report.ExitIP = geo.ip
		report.ExitCountry = geo.country
		report.ExitCity = geo.city
	}()
	go func() {
		defer wg.Done()
		item, geo := probeExit("", "exitip_direct", directGeoSources)
		items[4] = item
		report.ExitIPDirect = geo.ip
		report.ExitCountryDirect = geo.country
		report.ExitCityDirect = geo.city
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
type geoSource struct {
	url   string
	parse func(string) exitGeo
}

var geoSources = []geoSource{
	{"https://ipinfo.io/json", parseIPInfo},
	{"https://ipwho.is/", parseIPWho},
	{"https://www.cloudflare.com/cdn-cgi/trace", parseCloudflareTrace},
	{"https://api.ipify.org?format=json", parseIPOnly},
}

// directGeoSources serves the DIRECT path's echo. Every host here must be
// one the router sends direct: a foreign-named echo (cloudflare, ipinfo)
// lands in the proxy rule set and would report the proxy's exit — the
// exact opposite of what this leg measures. ipip.net answers with the
// address and a Chinese location line; 3322.org is a bare-address backup.
var directGeoSources = []geoSource{
	{"https://myip.ipip.net", parseIPIPText},
	{"http://members.3322.org/dyndns/getip", parseIPOnly},
}

// probeExit resolves the address the traffic actually leaves from, plus where
// that address is registered.
//
// The legs run one after another, so the wait is bounded by geoBudget: the
// report is only as fast as its slowest row, and one dead provider must not
// stall the other three.
func probeExit(proxyAddr, key string, sources []geoSource) (ProbeItem, exitGeo) {
	deadline := time.Now().Add(geoBudget)
	var (
		item ProbeItem
		geo  exitGeo
		got  bool // an address has been resolved
	)
	for i, src := range sources {
		if i > 0 && time.Now().After(deadline) {
			break
		}
		body := ""
		leg := probeHTTPGetWithin(key, src.url, proxyAddr, &body, geoLegTimeout)
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
		item = ProbeItem{Key: key}
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

// ipipRe pulls the address out of ipip.net's fixed line:
// "当前 IP:1.2.3.4  来自于:中国 广东 深圳  移动".
var ipipRe = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)

// parseIPIPText reads ipip.net's echo. The location text is Chinese-only,
// which is fine: this leg exists for the domestic exit, and "CN" is what
// the country can honestly be when the line says 中国.
func parseIPIPText(body string) exitGeo {
	ip := ipipRe.FindString(body)
	if ip == "" {
		return exitGeo{}
	}
	g := exitGeo{ip: ip}
	if !strings.Contains(body, "中国") {
		return g
	}
	g.country = "CN"
	if i := strings.Index(body, "来自于"); i >= 0 {
		loc := strings.Map(func(r rune) rune {
			if r == ':' || r == ' ' {
				return ' '
			}
			return r
		}, body[i+len("来自于"):])
		g.city = strings.TrimSpace(loc)
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
