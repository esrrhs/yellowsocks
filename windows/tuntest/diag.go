// diag.go carries the TUN pipeline diagnostics used by the privileged test
// driver: a deterministic probe sequence that exercises DNS (fake-ip), TCP
// relay and HTTPS end to end, plus a packet-capture helper that records the
// TCP handshake so a rejected connection can be traced to the side that
// resets it.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runPS executes a PowerShell script with a timeout and returns its combined
// output split into trimmed lines. Get-Net*/pktmon calls occasionally hang
// while the TUN adapter is churning, so every call is bounded.
func runPS(script string, timeout time.Duration) []string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", script).CombinedOutput()
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if err != nil {
		lines = append(lines, fmt.Sprintf("<powershell error: %v>", err))
	}
	return lines
}

// diagBlock prints a named PowerShell diagnostic section to the driver log.
func diagBlock(name, script string) {
	out("=== %s ===", name)
	for _, line := range runPS(script, 12*time.Second) {
		out("  | %s", line)
	}
}

// probeDNS resolves a name through the system resolver. Under TUN the adapter
// DNS points at the engine hijack, so a proxied name must come back as an
// address from the fake-ip pool (198.18.0.0/15) instead of a real one.
func probeDNS(name string) []net.IP {
	start := time.Now()
	ips, err := net.LookupIP(name)
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil {
		out("[driver] DNS %s FAIL after %v: %v", name, elapsed, err)
		return nil
	}
	parts := make([]string, 0, len(ips))
	for _, ip := range ips {
		parts = append(parts, ip.String())
	}
	out("[driver] DNS %s OK after %v: %s", name, elapsed, strings.Join(parts, ", "))
	return ips
}

// probeTCP dials host:port and reports the outcome. A plain IP dial isolates
// the TCP relay from DNS and from the HTTP layer.
func probeTCP(host string, port int, timeout time.Duration) error {
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, timeout)
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil {
		out("[driver] TCP %s FAIL after %v: %v", addr, elapsed, err)
		return err
	}
	_ = conn.Close()
	out("[driver] TCP %s OK after %v", addr, elapsed)
	return nil
}

// probeHTTP fetches a URL with no proxy configured, so the request traverses
// the TUN like any application's traffic.
func probeHTTP(url string, timeout time.Duration) error {
	client := &http.Client{Timeout: timeout}
	start := time.Now()
	resp, err := client.Get(url)
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil {
		out("[driver] HTTP %s FAIL after %v: %v", url, elapsed, err)
		return err
	}
	_ = resp.Body.Close()
	out("[driver] HTTP %s OK %d after %v", url, resp.StatusCode, elapsed)
	return nil
}

// probePipeline runs the full verdict sequence: DNS for a proxied name, then
// TCP to the resolved address (the fake-ip relay path), then HTTPS to both a
// proxied and a domestic site.
func probePipeline() {
	probeDNS("www.google.com")
	if ips := probeDNS("www.google.com"); len(ips) > 0 {
		v4 := ""
		for _, ip := range ips {
			if ip.To4() != nil {
				v4 = ip.String()
				break
			}
		}
		if v4 != "" {
			probeTCP(v4, 443, 8*time.Second)
		}
	}
	probeTCP("223.5.5.5", 80, 8*time.Second)
	probeHTTP("https://www.baidu.com", 15*time.Second)
	probeHTTP("https://www.google.com", 20*time.Second)
	out("[driver] PIPELINE probe done; packets=%s", tunStats())
}

// tunStats renders the live TUN counters maintained by the GUI app.
func tunStats() string { return app.TunSessionStats() }

// captureHandshake records a single TCP attempt in pktmon and prints every
// TCP line of the formatted trace. The flags column shows whether the peer
// answer (SYN,ACK) ever reached Windows and which side sent the RST.
func captureHandshake(label, host string, port int) {
	dir := exeDir()
	etl := filepath.Join(dir, "PktMon-diag.etl")
	txt := filepath.Join(dir, "pktmon-diag.txt")

	out("[driver] CAPTURE %s: preparando pktmon", label)
	for _, script := range []string{
		"pktmon stop",
		"pktmon filter remove",
		"pktmon status",
	} {
		out("  | $ %s", script)
		for _, line := range runPS(script, 20*time.Second) {
			out("  |   %s", line)
		}
	}

	startPS := fmt.Sprintf("pktmon start -c --comp nics -f %q -s 512", etl)
	out("  | $ %s", startPS)
	for _, line := range runPS(startPS, 25*time.Second) {
		out("  |   %s", line)
	}
	if info, err := os.Stat(etl); err != nil {
		out("[driver] CAPTURE %s: no etl (%v) — capture never started", label, err)
	} else {
		out("[driver] CAPTURE %s: etl created (%d bytes)", label, info.Size())
	}

	time.Sleep(500 * time.Millisecond)
	out("[driver] CAPTURE %s: dial %s:%d", label, host, port)
	err := probeTCP(host, port, 6*time.Second)
	time.Sleep(500 * time.Millisecond)

	for _, line := range runPS("pktmon stop", 20*time.Second) {
		out("  |   %s", line)
	}
	formatPS := fmt.Sprintf("pktmon format %q -o %q", etl, txt)
	for _, line := range runPS(formatPS, 30*time.Second) {
		out("  |   %s", line)
	}
	if info, serr := os.Stat(txt); serr != nil {
		out("[driver] CAPTURE %s: no formatted txt (%v)", label, serr)
	} else {
		out("[driver] CAPTURE %s: pktmon.txt %d bytes, dial err=%v", label, info.Size(), err)
	}
	for _, line := range runPS(fmt.Sprintf(
		"Get-Content -LiteralPath %q -Encoding Unicode -ErrorAction SilentlyContinue | Select-String -Pattern 'TCP|SYN|ACK|RST|Flags|198\\.18\\.0\\.1' | Select-Object -Last 60 | ForEach-Object { $_.Line }",
		txt), 30*time.Second) {
		out("  | %s", line)
	}
}

// exeDir returns the directory of the running executable: the driver pins its
// working directory there at startup.
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}
