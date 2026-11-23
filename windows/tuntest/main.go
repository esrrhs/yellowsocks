// tuntest is a privileged test driver for the TUN stack: it starts the
// engine from the local portable config and drives TUN mode by commands in
// tuntest-cmd.json, so repeated enable/disable/restart cycles never need
// another UAC prompt. Run elevated once; it restarts itself (inheriting the
// elevated token) whenever the command file asks for it.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/yellowsocks/guiapp"
)

const (
	cmdFile  = "tuntest-cmd.json"
	outLog   = "tuntest-out.log"
	errLog   = "tuntest-err.log"
	startTag = "[driver]"
)

var outFile *os.File

// app is the process-wide GUI app the diagnostics query for live TUN stats.
var app *guiapp.App

// out prints to the console and appends to tuntest-out.log so the test
// orchestrator can read results from outside the elevated console.
func out(format string, args ...interface{}) {
	line := fmt.Sprintf(format, args...)
	fmt.Println(line)
	if outFile != nil {
		stamp := time.Now().Format("15:04:05.000")
		fmt.Fprintf(outFile, "%s %s\n", stamp, line)
	}
}

type command struct {
	Op string `json:"op"` // enable | disable | status | probe | capture | netdiag | restart | quit
	// Host/Port name the first capture target, Host2/Port2 an optional
	// second one (used to compare a proxied flow with a direct flow).
	Host  string `json:"host,omitempty"`
	Port  int    `json:"port,omitempty"`
	Host2 string `json:"host2,omitempty"`
	Port2 int    `json:"port2,omitempty"`
	// Script is executed verbatim by the "ps" diagnostic op.
	Script string `json:"script,omitempty"`
}

func main() {
	exe, _ := os.Executable()
	// Pin the CWD to the exe directory FIRST: out.log, the command file and
	// loggo's files must live where the orchestrator looks for them, no
	// matter how the elevated process was started (an elevated ShellExecute
	// resets the CWD to System32). The exe directory is writable, so
	// initializing loggo after the chdir avoids its unwritable-CWD panic.
	if dir := filepath.Dir(exe); dir != "" {
		_ = os.Chdir(dir)
	}
	loggo.Ini(loggo.Config{Level: loggo.NameToLevel("info"), Prefix: "tuntest"})
	defer func() {
		if r := recover(); r != nil {
			stk := string(debug.Stack())
			out("%s PANIC: %v", startTag, r)
			out("%s", stk)
			outFile.Close()
			os.Exit(2)
		}
	}()

	if f, err := os.OpenFile(outLog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		outFile = f
		defer f.Close()
	}
	// Capture stderr: the tun2socks engine calls log.Fatalf (os.Exit) on
	// start failures and gvisor panics print to stderr — without this the
	// failure reason dies with the (closable) console window.
	if ef, err := os.OpenFile(errLog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		os.Stderr = ef
		defer ef.Close()
	}
	out("%s started pid=%d exe=%s", startTag, os.Getpid(), exe)

	a := guiapp.NewApp()
	app = a
	a.Startup(context.Background())

	// Kill stale driver instances first: they keep the engine ports bound, so
	// the new engine fails with "Only one usage of each socket address" and
	// every probe then measures a dead engine. This process is elevated, so it
	// can end them, and only survives itself.
	killPS := fmt.Sprintf("Get-Process yellowsocks-tuntest -ErrorAction SilentlyContinue | Where-Object { $_.Id -ne %d } | Stop-Process -Force", os.Getpid())
	if output, kerr := exec.Command("powershell", "-NoProfile", "-Command", killPS).CombinedOutput(); kerr != nil {
		out("%s warning: killing stale instances: %v: %s", startTag, kerr, strings.TrimSpace(string(output)))
	}

	// A previous driver instance may still hold the engine ports: wait for
	// them to close before starting, so exactly one live engine exists.
	for _, port := range []string{"127.0.0.1:1053", "127.0.0.1:10808", "127.0.0.1:8080"} {
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			ln, err := net.Listen("tcp", port)
			if err == nil {
				_ = ln.Close()
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
	}

	if err := a.StartEngine(); err != nil {
		// Startup's autostart goroutine may have started the engine
		// already (settings auto_start): that is success, not failure.
		if !strings.Contains(err.Error(), "already running") {
			out("%s ENGINE_FAIL: %v", startTag, err)
			outFile.Close()
			os.Exit(1)
		}
		out("%s ENGINE_OK (already running from startup autostart)", startTag)
	}
	st := a.EngineStatus()
	out("%s ENGINE_STATE running=%v socks5=%s dns=%s", startTag, st.Running, st.Socks5, st.DNS)

	// Bring TUN up right away (the whole point of the driver). Disable first
	// so any autostarted or adapter-left-up state is cleared and the
	// subsequent applyTun re-runs from scratch (applyTun is a no-op when the
	// adapter is already up).
	//
	// The production watchdog stays ENABLED, and a lease on top of it bounds
	// how long the adapter may stay up at all. A broken TUN black-holes the
	// machine, and a dead diagnostic driver must never be able to keep that
	// state: even if the orchestrator stops talking to this process, the
	// lease expires and the network comes back on its own.
	// A bounded soak keeps the adapter up long enough to probe the pipeline,
	// while the watchdog still rolls it back if nothing works — the lease
	// below is the second, independent stop.
	guiapp.SetTunWatchdogSoak(30 * time.Second)
	setTun(a, false)
	armLease(tunLease)
	setTun(a, true)
	go leaseWatchdog(a)

	// Self-test: the driver's own traffic enters the TUN like any other
	// app, so the probe sequence exercises DNS hijack + fake-ip + TCP relay
	// + engine routing exactly as a real request would.
	go func() {
		time.Sleep(2 * time.Second)
		probePipeline()
	}()

	go func() {
		time.Sleep(6 * time.Second)
		diagBlock("AdapterStats", "Get-NetAdapterStatistics -InterfaceAlias YellowSocks -ErrorAction SilentlyContinue | Format-List ReceivedBytes,SentBytes,OutboundDiscardedPackets | Out-String -Width 200")
		diagBlock("Neighbor", "Get-NetNeighbor -InterfaceAlias YellowSocks -ErrorAction SilentlyContinue | Format-Table IPAddress,State,LinkLayerAddress -AutoSize | Out-String -Width 150")
		diagBlock("TunRoutes", "Get-NetRoute -InterfaceAlias YellowSocks -ErrorAction SilentlyContinue | Format-Table DestinationPrefix,NextHop,RouteMetric,InterfaceMetric,State -AutoSize | Out-String -Width 220")
		diagBlock("DnsServers", "Get-DnsClientServerAddress -InterfaceAlias YellowSocks -ErrorAction SilentlyContinue | Format-Table InterfaceAlias,AddressFamily,ServerAddresses -AutoSize | Out-String -Width 160")
		diagBlock("IpInterface", "Get-NetIPInterface -InterfaceAlias YellowSocks -ErrorAction SilentlyContinue | Format-Table AddressFamily,ConnectionState,InterfaceMetric,Forwarding,NlMtu -AutoSize | Out-String -Width 160")
	}()

	for {
		data, err := os.ReadFile(cmdFile)
		if err == nil {
			var cmd command
			if json.Unmarshal(data, &cmd) == nil && cmd.Op != "" {
				_ = os.Remove(cmdFile)
				switch cmd.Op {
				case "disable":
					setTun(a, false)
				case "enable":
					armLease(tunLease)
					setTun(a, true)
				case "netdiag":
					// pktmon FIRST: the Get-Net* powershell calls sometimes
					// hang during TUN churn and would starve this diagnostic.
					probePipeline()
					diagBlock("route-for-peer", "Find-NetRoute -RemoteIPAddress 198.18.0.2 | Select-Object IPAddress,InterfaceAlias | Format-Table -AutoSize | Out-String -Width 150")
					diagBlock("neighbor", "Get-NetNeighbor -InterfaceAlias YellowSocks -ErrorAction SilentlyContinue | Format-Table IPAddress,State,LinkLayerAddress -AutoSize | Out-String -Width 150")
					diagBlock("ipif", "Get-NetIPInterface -InterfaceAlias YellowSocks | Format-Table AddressFamily,ConnectionState,InterfaceMetric,Dhcp,Forwarding -AutoSize | Out-String -Width 150")
					out("=== NETDIAG packet counters: %s ===", a.TunSessionStats())
				case "probe":
					probePipeline()
				case "ps":
					// Diagnostic escape hatch: run one bounded PowerShell
					// script so an elevated test can toggle a Windows setting
					// (firewall profile, offload, network category) and prove
					// whether it is what blocks the TUN pipeline.
					for _, line := range runPS(cmd.Script, 90*time.Second) {
						out("  | %s", line)
					}
				case "capture":
					// Capture a handshake the pipeline itself initiates: a
					// fresh proxied name resolves to a fake-ip, and the dial to
					// it is the exact flow that fails. pktmon records whether
					// the SYN-ACK ever reached Windows and who sent the RST.
					captureHandshake("fake-ip", cmd.Host, cmd.Port)
					if cmd.Host2 != "" {
						captureHandshake("direct", cmd.Host2, cmd.Port2)
					}
				case "status":
					st := a.EngineStatus()
					out("%s STATUS running=%v tun=%v tunErr=%q sysproxy=%v packets=%s",
						startTag, st.Running, st.Tun, st.TunError, st.SystemProxy, a.TunSessionStats())
				case "restart":
					out("%s restarting self (elevated child)", startTag)
					child := exec.Command(exe)
					child.Stdout, child.Stderr = os.Stdout, os.Stderr
					_ = child.Start()
					a.Shutdown(context.Background())
					os.Exit(0)
				case "quit":
					out("%s quitting", startTag)
					a.Shutdown(context.Background())
					os.Exit(0)
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// tunLease bounds how long the adapter may stay up in a diagnostics run. The
// pipeline watchdog already rolls back a dead pipeline within seconds, but it
// only probes the network path: a driver that hangs, is killed, or is simply
// forgotten must not leave the machine black-holed either.
const tunLease = 45 * time.Second

// leaseUntil holds the lease deadline as unix nanoseconds (0 = no lease).
var leaseUntil atomic.Int64

// armLease extends the lease from now.
func armLease(d time.Duration) {
	leaseUntil.Store(time.Now().Add(d).UnixNano())
}

// leaseWatchdog tears the TUN down when the lease expires, then keeps
// guarding: an enable issued by the command file extends the lease first.
func leaseWatchdog(a *guiapp.App) {
	for {
		time.Sleep(time.Second)
		st := a.EngineStatus()
		if !st.Tun {
			continue
		}
		until := leaseUntil.Load()
		if until != 0 && time.Now().UnixNano() < until {
			continue
		}
		out("%s LEASE expired: tearing TUN down so the network recovers", startTag)
		setTun(a, false)
	}
}

func setTun(a *guiapp.App, on bool) {
	start := time.Now()
	err := a.SetTunMode(on)
	st := a.EngineStatus()
	if err != nil {
		out("%s TUN set %v FAIL after %v: %v (status tun=%v tunErr=%q)",
			startTag, on, time.Since(start).Round(time.Millisecond), err, st.Tun, st.TunError)
		return
	}
	out("%s TUN set %v OK after %v (status tun=%v)",
		startTag, on, time.Since(start).Round(time.Millisecond), st.Tun)
}
