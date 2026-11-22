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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
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
	Op string `json:"op"` // enable | disable | status | restart | quit
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
	a.Startup(context.Background())

	// A previous driver instance may still hold the engine ports: wait for
	// them to close before starting, so exactly one live engine exists.
	for _, port := range []string{"127.0.0.1:1053", "127.0.0.1:10808", "127.0.0.1:8080"} {
		deadline := time.Now().Add(15 * time.Second)
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

	// Bring TUN up right away (the whole point of the driver), with a long
	// watchdog soak so diagnostics can inspect the live adapter. Disable
	// first so any autostarted or adapter-left-up state is cleared and the
	// subsequent applyTun re-runs with this process's soak value (applyTun
	// is a no-op when the adapter is already up).
	guiapp.SetTunWatchdogSoak(10 * time.Minute)
	setTun(a, false)
	setTun(a, true)

	// Self-test: the driver's own traffic enters the TUN like any other
	// app, so one HTTPS round-trip through the whole pipeline tells whether
	// DNS hijack + TCP relay + engine routing all work. The diagnostics
	// first snapshot exactly what Windows thinks about the routes.
	// The pipeline verdict runs FIRST and unblocked; the deeper diagnostics
	// run afterwards in their own goroutine (some Get-Net* powershell calls
	// hang during TUN churn and must not mask the result).
	go func() {
		time.Sleep(2 * time.Second)
		// IP-literal probe first: isolates DNS from the TCP relay.
		probeStart := time.Now()
		conn, perr := net.DialTimeout("tcp", "223.5.5.5:80", 8*time.Second)
		if perr != nil {
			out("%s SELFTEST tcp 223.5.5.5:80 FAIL after %v: %v", startTag, time.Since(probeStart).Round(time.Millisecond), perr)
		} else {
			conn.Close()
			out("%s SELFTEST tcp 223.5.5.5:80 OK in %v", startTag, time.Since(probeStart).Round(time.Millisecond))
		}
		for _, url := range []string{"https://www.baidu.com", "https://www.google.com"} {
			start := time.Now()
			client := &http.Client{Timeout: 20 * time.Second}
			resp, err := client.Get(url)
			if err != nil {
				out("%s SELFTEST %s FAIL after %v: %v", startTag, url, time.Since(start).Round(time.Millisecond), err)
				continue
			}
			resp.Body.Close()
			out("%s SELFTEST %s OK HTTP %d in %v", startTag, url, resp.StatusCode, time.Since(start).Round(time.Millisecond))
		}
	}()

	go func() {
		time.Sleep(6 * time.Second)
		diag := func(name string, script string) {
			out("=== DIAG %s ===", name)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", script)
			output, err := cmd.CombinedOutput()
			if err != nil {
				out("  | powershell error: %v", err)
			}
			for _, line := range strings.Split(string(output), "\n") {
				line = strings.TrimRight(line, "\r")
				if line != "" {
					out("  | %s", line)
				}
			}
		}
		diag("AdapterStats", "Get-NetAdapterStatistics -InterfaceAlias YellowSocks -ErrorAction SilentlyContinue | Format-List ReceivedBytes,SentBytes,OutboundDiscardedPackets | Out-String -Width 200")
		diag("Neighbor", "Get-NetNeighbor -InterfaceAlias YellowSocks -ErrorAction SilentlyContinue | Format-Table IPAddress,State,LinkLayerAddress -AutoSize | Out-String -Width 150")
		diag("TunRoutes", "Get-NetRoute -InterfaceAlias YellowSocks -ErrorAction SilentlyContinue | Format-Table DestinationPrefix,NextHop,RouteMetric,InterfaceMetric,State -AutoSize | Out-String -Width 220")
		diag("Pktmon", "pktmon stop | Out-Null; pktmon filter remove | Out-Null; pktmon filter add ysProbe -i 223.5.5.5 | Out-Null; pktmon start -c --comp nics | Out-Null; Test-Connection -ComputerName 223.5.5.5 -Count 2 -ErrorAction SilentlyContinue | Out-Null; Start-Sleep 1; pktmon stop | Out-Null; pktmon format PktMon.etl -o pktmon.txt | Out-Null; Get-Content pktmon.txt -ErrorAction SilentlyContinue | Out-String -Width 220")
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
					setTun(a, true)
				case "netdiag":
					// pktmon FIRST: the Get-Net* powershell calls sometimes
					// hang during TUN churn and would starve this diagnostic.
					diag := func(name string, script string) {
						out("=== NETDIAG %s ===", name)
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", script)
						output, err := cmd.CombinedOutput()
						if err != nil {
							out("  | powershell error: %v", err)
						}
						for _, line := range strings.Split(string(output), "\n") {
							line = strings.TrimRight(line, "\r")
							if line != "" {
								out("  | %s", line)
							}
						}
					}
					diag("pktmon-223.5.5.5", "pktmon stop | Out-Null; pktmon filter remove | Out-Null; pktmon filter add ys -i 223.5.5.5 | Out-Null; pktmon start -c --comp nics | Out-Null; Test-Connection -ComputerName 223.5.5.5 -Count 2 -ErrorAction SilentlyContinue | Out-Null; Start-Sleep 1; pktmon stop | Out-Null; pktmon format PktMon.etl -o pktmon.txt | Out-Null; Get-Content pktmon.txt -ErrorAction SilentlyContinue | Out-String -Width 220")
					diag("route-for-peer", "Find-NetRoute -RemoteIPAddress 198.18.0.2 | Select-Object IPAddress,InterfaceAlias | Format-Table -AutoSize | Out-String -Width 150")
					diag("neighbor", "Get-NetNeighbor -InterfaceAlias YellowSocks -ErrorAction SilentlyContinue | Format-Table IPAddress,State,LinkLayerAddress -AutoSize | Out-String -Width 150")
					diag("ipif", "Get-NetIPInterface -InterfaceAlias YellowSocks | Format-Table AddressFamily,ConnectionState,InterfaceMetric,Dhcp,Forwarding -AutoSize | Out-String -Width 150")
					out("=== NETDIAG packet counters: %s ===", a.TunSessionStats())
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
