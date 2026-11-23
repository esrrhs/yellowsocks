// scenario.go is the one-shot TUN acceptance run: it brings the adapter up,
// walks the exact paths a user request takes (raw-socket DNS to a captured
// resolver, the system resolver, a TCP dial to a fake-ip, HTTPS to a proxied
// and a domestic site), traces the fake-ip handshake in pktmon and tears the
// adapter down again. Everything is bounded so a hung step cannot keep the
// machine off the network: the lease watchdog and the hard timer at the end
// of runScenario both stop the TUN.
package main

import (
	"fmt"
	"net"
	"os"
	"strings"

	"context"
	"time"

	"github.com/esrrhs/yellowsocks/guiapp"
	"github.com/miekg/dns"
)

// scenarioProxyDNS is the resolver DNS take-over publishes on every
// interface, so this is where a captured :53 datagram is addressed.
const scenarioProxyDNS = "223.5.5.5:53"

// runScenario drives the whole TUN acceptance sequence and exits. It is meant
// for a single elevated launch: bring up, measure, tear down, report.
func runScenario(a *guiapp.App) {
	out("[driver] === SCENARIO start ===")
	// Independent hard stop: even if a probe hangs, the adapter goes down.
	time.AfterFunc(210*time.Second, func() {
		out("[driver] SCENARIO hard timeout: forcing TUN down")
		_ = a.SetTunMode(false)
		time.Sleep(2 * time.Second)
		os.Exit(3)
	})

	setTun(a, false)
	out("[driver] SCENARIO disable done")
	armLease(150 * time.Second)
	start := time.Now()
	setTun(a, true)
	out("[driver] SCENARIO enable call returned after %v", time.Since(start).Round(time.Millisecond))
	if !a.EngineStatus().Tun {
		out("[driver] SCENARIO aborted: TUN did not come up")
		finish(a)
		return
	}
	out("[driver] SCENARIO TUN up in %v", time.Since(start).Round(time.Millisecond))

	// Give the stack a moment, then probe repeatedly: the first queries right
	// after bring-up race the resolver-cache flush.
	var googleIP string
	for attempt := 1; attempt <= 4; attempt++ {
		time.Sleep(2 * time.Second)
		out("[driver] --- attempt %d ---", attempt)
		queryViaSocket("www.google.com")
		queryViaSocket("www.baidu.com")
		if ips := probeDNS("www.google.com"); len(ips) > 0 {
			for _, ip := range ips {
				if v4 := ip.To4(); v4 != nil {
					googleIP = v4.String()
					break
				}
			}
		}
		if googleIP != "" {
			break
		}
	}

	if googleIP != "" {
		out("[driver] fake ip for www.google.com: %s", googleIP)
		captureFlow("google", googleIP, func() {
			probeTCP(googleIP, 443, 6*time.Second)
		})
		probeHTTP("https://www.google.com", 20*time.Second)
	} else {
		out("[driver] no fake ip: skipping TCP/TLS probes")
	}
	probeHTTP("https://www.baidu.com", 15*time.Second)
	probeTCP("223.5.5.5", 80, 8*time.Second)

	diagBlock("AdapterStats", "Get-NetAdapterStatistics -InterfaceAlias YellowSocks -ErrorAction SilentlyContinue | Format-List ReceivedBytes,SentBytes,OutboundDiscardedPackets,OutboundPacketsWithNoRoute | Out-String -Width 200")
	diagBlock("Neighbor", "Get-NetNeighbor -InterfaceAlias YellowSocks -ErrorAction SilentlyContinue | Format-Table IPAddress,State,LinkLayerAddress -AutoSize | Out-String -Width 150")
	diagBlock("IpInterface", "Get-NetIPInterface -InterfaceAlias YellowSocks -ErrorAction SilentlyContinue | Format-List AddressFamily,ConnectionState,InterfaceMetric,Forwarding,NlMtu,WeakHostReceive,WeakHostSend | Out-String -Width 200")
	out("=== NETDIAG counters: %s ===", a.TunSessionStats())

	finish(a)
}

// finish tears the adapter down and exits.
func finish(a *guiapp.App) {
	setTun(a, false)
	out("[driver] === SCENARIO end (tun=%v) ===", a.EngineStatus().Tun)
	a.Shutdown(context.Background())
	time.Sleep(500 * time.Millisecond)
	os.Exit(0)
}

// queryViaSocket sends a DNS query from an ordinary UDP socket to the
// hijacked resolver. Unlike net.LookupIP it bypasses the Windows resolver
// cache and the multi-homed heuristic, so a failure here means the captured
// :53 path itself is broken (query in, reply out).
func queryViaSocket(name string) {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	m.RecursionDesired = true

	conn, err := net.DialTimeout("udp", scenarioProxyDNS, 3*time.Second)
	if err != nil {
		out("[driver] SOCKET-DNS %s dial: %v", name, err)
		return
	}
	defer conn.Close()
	payload, err := m.Pack()
	if err != nil {
		return
	}
	start := time.Now()
	if _, err := conn.Write(payload); err != nil {
		out("[driver] SOCKET-DNS %s write: %v", name, err)
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil {
		out("[driver] SOCKET-DNS %s FAIL after %v: %v", name, elapsed, err)
		return
	}
	resp := new(dns.Msg)
	if err := resp.Unpack(buf[:n]); err != nil {
		out("[driver] SOCKET-DNS %s unparsable (%d bytes): %v", name, n, err)
		return
	}
	var answers []string
	for _, rr := range resp.Answer {
		if a, ok := rr.(*dns.A); ok {
			answers = append(answers, a.A.String())
		}
	}
	out("[driver] SOCKET-DNS %s OK after %v rcode=%s answers=%s",
		name, elapsed, dns.RcodeToString[resp.Rcode], strings.Join(answers, ","))
}

// captureFlow traces every packet to and from ip while action runs. Filtering
// on the flow address keeps the trace readable: it shows the SYN, whether the
// SYN-ACK reached the miniport, and whether the host stack ever answered it.
func captureFlow(label, ip string, action func()) {
	dir := exeDir()
	etl := fmt.Sprintf("%s/PktMon-%s.etl", dir, label)
	txt := fmt.Sprintf("%s/pktmon-%s.txt", dir, label)
	for _, script := range []string{
		"pktmon stop",
		"pktmon filter remove",
		fmt.Sprintf("pktmon filter add ys-%s -i %s", label, ip),
		fmt.Sprintf("pktmon start -c --comp all -f %q -s 8", etl),
	} {
		out("  | $ %s", script)
		for _, line := range runPS(script, 20*time.Second) {
			out("  |   %s", line)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if action != nil {
		action()
	}
	time.Sleep(300 * time.Millisecond)
	for _, line := range runPS("pktmon stop", 20*time.Second) {
		out("  |   %s", line)
	}
	for _, line := range runPS(fmt.Sprintf("pktmon format %q -o %q", etl, txt), 30*time.Second) {
		out("  |   %s", line)
	}
	out("=== pktmon flow %s (%s) ===", label, ip)
	for _, line := range runPS(fmt.Sprintf(
		"Get-Content -LiteralPath %q -Encoding Unicode -ErrorAction SilentlyContinue | Select-String -Pattern %q | Select-Object -Last 40 | ForEach-Object { $_.Line }",
		txt, ip), 30*time.Second) {
		out("  | %s", line)
	}
}

