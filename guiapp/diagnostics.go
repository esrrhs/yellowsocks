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

// ProbeReport is what the UI renders: the individual legs plus the exit IP,
// which is the only way to tell "the proxy works" from "the proxy works but
// egresses from somewhere useless".
type ProbeReport struct {
	At     int64       `json:"at"`
	ExitIP string      `json:"exit_ip"`
	Items  []ProbeItem `json:"items"`
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
		body := ""
		item := probeHTTPGet("exitip", "https://api.ipify.org?format=json", httpAddr, &body)
		items[3] = item
		if item.OK {
			if ip := parseIPify(body); ip != "" {
				report.ExitIP = ip
				items[3].Detail = ip
			}
		}
	}()

	wg.Wait()
	report.Items = items
	return report
}

// probeHTTPGet performs one GET and reports status and elapsed time. proxyAddr is
// the local HTTP proxy ("host:port"), empty for a direct request. When body is
// non-nil up to 4KB of the response is stored in it.
func probeHTTPGet(key, rawURL, proxyAddr string, body *string) ProbeItem {
	item := ProbeItem{Key: key, Target: rawURL}
	tr := &http.Transport{
		// No keep-alives: a reused connection would report the handshake of a
		// previous probe instead of this one.
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: probeTimeout,
		DialContext:         (&net.Dialer{Timeout: dialTimeout}).DialContext,
	}
	if proxyAddr != "" {
		if pu, err := url.Parse("http://" + proxyAddr); err == nil {
			tr.Proxy = http.ProxyURL(pu)
		}
	}
	client := &http.Client{Transport: tr, Timeout: probeTimeout}

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
