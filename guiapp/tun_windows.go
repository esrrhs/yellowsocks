//go:build windows

package guiapp

import (
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/yellowsocks/core/netdial"
	tun2socks_engine "github.com/xjasonlyu/tun2socks/v2/engine"
	"golang.org/x/sys/windows"
	wintun "golang.zx2c4.com/wintun"
	wgwinipcfg "golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

// probeURL is a domestic HTTPS endpoint: a full GET crosses the whole
// pipeline (DNS via the adapter DNS through the TUN → capture → SOCKS5 →
// engine inbound → direct dial bound to the physical NIC) and comes back,
// matching what a real user request exercises.
const probeURL = "https://www.baidu.com"

//go:embed tun/wintun_amd64.dll
var wintunDLL []byte

// tunGUID is the fixed device-instance GUID embedded in the engine's device
// URL: wintun reuses the adapter with the same GUID across runs instead of
// accumulating duplicates, and the SwDevice is bound to the creating
// process — when that process dies the device (and its takeover routes)
// are removed automatically, so crashes self-heal the network.
var tunGUID = windows.GUID{
	Data1: 0x5f6c2a91,
	Data2: 0x8b3d,
	Data3: 0x4e7a,
	Data4: [8]byte{0xa2, 0x14, 0x3c, 0x9d, 0x77, 0xe1, 0x0b, 0x46},
}

// tunManager owns the tun2socks engine lifecycle and the Windows routing
// state around it.
type tunManager struct {
	mu         sync.Mutex
	active     bool
	stopped    atomic.Bool
	openHandle *wintun.Adapter // keeps our OpenAdapter reference alive
	luid       wgwinipcfg.LUID
	physIfIdx  uint32
	physLUID   wgwinipcfg.LUID
	physGw     netip.Addr
	upstreamRT []netip.Prefix // /32 routes for upstream servers on the physical adapter
}

var manager = &tunManager{}

func tunEnable(params TunParams) error { return manager.enable(params) }
func tunDisable() error                { return manager.disable() }
func tunIsActive() bool                { return manager.isActive() }

// tunSessionStatsLive returns live TUN diagnostics for the driver/GUI.
func tunSessionStatsLive() string {
	m := manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active {
		return "inactive"
	}
	return fmt.Sprintf("physIfIdx=%d upstreamExceptions=%d", m.physIfIdx, len(m.upstreamRT))
}

func (m *tunManager) isActive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

func (m *tunManager) enable(params TunParams) (err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var perr error
	if m.active {
		return errors.New("TUN mode already active")
	}
	m.stopped.Store(false)

	// 1. wintun.dll on disk: the standalone wintun module loads it with
	// LoadLibraryEx from the application directory (or System32).
	if err := ensureWintunDLL(); err != nil {
		return fmt.Errorf("prepare wintun.dll: %w", err)
	}

	// 2. The tun2socks engine creates the wintun adapter itself (fixed
	// GUID via the device URL query, so runs reuse the same adapter),
	// the gvisor stack, and forwards every captured flow into the engine's
	// SOCKS5 inbound — the same proxy Clash Verge's kernel forwards into.
	// NOTE: do NOT create the adapter separately before this — a second
	// CreateAdapter for the same name/GUID conflicts and the engine's
	// start() failure path calls log.Fatalf (os.Exit), killing the GUI.
	// The engine creates a fresh wintun adapter (random GUID) each run; the
	// SwDevice is process-bound, so the adapter — and its takeover routes —
	// are removed automatically when this process exits, self-healing any
	// leftover state.
	tun2socks_engine.Insert(&tun2socks_engine.Key{
		Device:     "tun://" + tunAdapterName,
		Proxy:      "socks5://" + params.Socks5Addr,
		LogLevel:   "debug",
		MTU:        tunMTU,
		UDPTimeout: 60 * time.Second,
	})
	tun2socks_engine.Start()

	// 3. The adapter now exists: grab its LUID for the routing config.
	// OpenAdapter keeps a second handle open; NEVER close it while the
	// engine runs (WintunCloseAdapter removes the adapter instance). The
	// leaked handle is released at process exit, after which the SwDevice
	// — bound to this process — is removed too, self-healing leftovers.
	adapterDev, err := wintun.OpenAdapter(tunAdapterName)
	if err != nil {
		m.teardownLocked()
		return fmt.Errorf("open TUN adapter: %w", err)
	}
	m.openHandle = adapterDev
	luid := adapterDev.LUID()
	tunLUID := wgwinipcfg.LUID(luid)
	m.luid = tunLUID

	// 4. Addresses FIRST: routes added before the interface owns an address
	// sit in a half-valid state that Windows never uses for delivery. The
	// /30 gives the adapter a connected subnet whose PEER (198.18.0.2)
	// serves as the route gateway and the advertised DNS — sing-tun's
	// Windows configuration exactly.
	addresses := []netip.Prefix{
		netip.MustParsePrefix(tunIPv4 + "/30"),
	}
	if err := tunLUID.SetIPAddresses(addresses); err != nil {
		m.teardownLocked()
		return fmt.Errorf("assign TUN address: %w", err)
	}

	// 5. Interface tuning (mirrors wireguard-windows / sing-tun):
	// forwarding on, router discovery and DAD off (the address must be
	// usable instantly, not sit tentative while traffic black-holes), fixed
	// MTU, and an interface metric of 0 so the takeover routes win
	// immediately.
	ipif, err := tunLUID.IPInterface(wgwinipcfg.AddressFamily(windows.AF_INET))
	if err != nil {
		m.teardownLocked()
		return fmt.Errorf("read IP interface: %w", err)
	}
	ipif.ForwardingEnabled = true
	ipif.RouterDiscoveryBehavior = wgwinipcfg.RouterDiscoveryDisabled
	ipif.DadTransmits = 0
	ipif.ManagedAddressConfigurationSupported = false
	ipif.OtherStatefulConfigurationSupported = false
	ipif.NLMTU = tunMTU
	ipif.UseAutomaticMetric = false
	ipif.Metric = 0
	if err := ipif.Set(); err != nil {
		m.teardownLocked()
		return fmt.Errorf("apply interface tuning: %w", err)
	}

	// 6. Permanent v4 neighbor entry for the gateway peer: wintun is a pure
	// L3 adapter (NdisMediumIP, no ARP support), so without this entry
	// Windows queues every unicast packet behind a neighbor probe that
	// never resolves and the TUN silently black-holes all traffic.
	gateway := netip.MustParseAddr(tunDNSv4) // 198.18.0.2 peer
	ps := fmt.Sprintf("New-NetNeighbor -InterfaceAlias '%s' -IPAddress %s -LinkLayerAddress %s -State Permanent -PolicyStore ActiveStore -ErrorAction Stop",
		tunAdapterName, tunDNSv4, tunGatewayMAC4)
	if output, nerr := exec.Command("powershell", "-NoProfile", "-Command", ps).CombinedOutput(); nerr != nil {
		loggo.Warn("[TUN] permanent neighbor %s: %v: %s", tunDNSv4, nerr, string(output))
	}

	// 7. Default-route takeover via two half-routes through the gateway
	// peer (sing-tun's Windows configuration: full 0.0.0.0/0 equivalent
	// split into two /1s so the physical default route stays in place as a
	// fallback when the TUN goes down).
	halfs := []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/1"),
		netip.MustParsePrefix("128.0.0.0/1"),
	}
	routesFor := func() []*wgwinipcfg.RouteData {
		var out []*wgwinipcfg.RouteData
		for _, half := range halfs {
			out = append(out, &wgwinipcfg.RouteData{
				Destination: half,
				NextHop:     gateway,
				Metric:      0,
			})
		}
		return out
	}
	// SetRoutesForFamily would fail with ERROR_NOT_FOUND on a freshly
	// created adapter (its internal flush finds no route rows yet): flush
	// tolerantly, then plain-add the takeover routes.
	_ = tunLUID.FlushRoutes(wgwinipcfg.AddressFamily(windows.AF_INET))
	if err := tunLUID.AddRoutes(routesFor()); err != nil {
		m.teardownLocked()
		return fmt.Errorf("add v4 routes: %w", err)
	}

	// 8. Adapter DNS: a real public DNS reachable through the pipeline.
	// The engine forwards DNS queries like any UDP flow: TUN capture →
	// SOCKS5 UDP relay → engine inbound → Decide(223.5.5.5) → direct dial
	// (a CN IP) → real answer. The engine's smart DNS split applies to
	// every resolver query system-wide.
	if err := tunLUID.SetDNS(
		wgwinipcfg.AddressFamily(windows.AF_INET),
		[]netip.Addr{netip.MustParseAddr(tunProbeDNS)}, []string{},
	); err != nil {
		m.teardownLocked()
		return fmt.Errorf("set TUN DNS: %w", err)
	}
	_ = tunLUID.FlushDNS(wgwinipcfg.AddressFamily(windows.AF_INET))

	// 9. Pin the engine's own dials (direct relays, DNS upstreams,
	// upstream sockets) to the physical adapter: without the bind they
	// would follow the takeover routes back into the TUN and loop.
	m.physIfIdx, m.physGw, perr = physicalDefaultRoute(tunLUID)
	if perr != nil {
		m.teardownLocked()
		return fmt.Errorf("find physical default route: %w", perr)
	}
	m.physLUID, perr = wgwinipcfg.LUIDFromIndex(m.physIfIdx)
	if perr != nil {
		m.teardownLocked()
		return fmt.Errorf("resolve physical interface LUID: %w", perr)
	}
	netdial.SetBindInterface(m.physIfIdx)

	// 10. /32 exceptions for the proxy's own upstream servers: the SPP
	// library dials from code where no socket option can be injected, so
	// those flows are kept off the TUN at the routing layer.
	for _, ipStr := range params.UpstreamIPs {
		ip := netip.MustParseAddr(ipStr)
		prefix := netip.PrefixFrom(ip, ip.BitLen())
		if err := m.physLUID.AddRoute(prefix, m.physGw, 1); err != nil {
			loggo.Warn("[TUN] upstream /32 route %s via %s: %v", prefix, m.physGw, err)
			continue
		}
		m.upstreamRT = append(m.upstreamRT, prefix)
	}

	m.active = true
	loggo.Info("[TUN] adapter %s up (%s/30, MTU %d); physical iface #%d via %s; upstream exceptions: %d",
		tunAdapterName, tunIPv4, tunMTU, m.physIfIdx, m.physGw, len(m.upstreamRT))
	m.startWatchdog(params)
	return nil
}

func (m *tunManager) disable() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active {
		return nil
	}
	m.teardownLocked()
	loggo.Info("[TUN] adapter %s down", tunAdapterName)
	return nil
}

// teardownLocked reverses enable() step by step. Safe on partially built
// state: everything below tolerates nils. Callers hold m.mu.
func (m *tunManager) teardownLocked() {
	m.stopped.Store(true)
	netdial.SetBindInterface(0)

	// 1. Routing state down first: nothing new may be routed into the dead
	// pipeline while the unwind continues.
	if m.luid != 0 {
		family := wgwinipcfg.AddressFamily(windows.AF_INET)
		_ = m.luid.FlushRoutes(family)      // removes the /1 takeover routes
		_ = m.luid.FlushIPAddresses(family) // removes 198.18.0.1
		_ = m.luid.FlushDNS(family)
		m.luid = 0
	}
	for _, prefix := range m.upstreamRT {
		if err := m.physLUID.DeleteRoute(prefix, m.physGw); err != nil {
			loggo.Warn("[TUN] delete upstream route %s: %v", prefix, err)
		}
	}
	m.upstreamRT = nil

	// 2. Stop the engine: closes the gvisor stack and the adapter handle
	// (which removes the adapter instance — belt and suspenders after the
	// explicit flush above).
	tun2socks_engine.Stop()

	// 3. Drop our own adapter reference (the engine's close already
	// removed the adapter; this close is a harmless double-remove).
	if m.openHandle != nil {
		_ = m.openHandle.Close()
		m.openHandle = nil
	}
	m.active = false
}

// physicalDefaultRoute finds the current default IPv4 route (0.0.0.0/0)
// excluding our own adapter; its interface is where the engine's sockets
// must bind, and its next hop is the gateway for the upstream /32 routes.
func physicalDefaultRoute(exclude wgwinipcfg.LUID) (uint32, netip.Addr, error) {
	rows, err := wgwinipcfg.GetIPForwardTable2(wgwinipcfg.AddressFamily(windows.AF_INET))
	if err != nil {
		return 0, netip.Addr{}, err
	}
	found := false
	var bestMetric uint32
	var bestRow wgwinipcfg.MibIPforwardRow2
	for i := range rows {
		r := &rows[i]
		if r.InterfaceLUID == exclude {
			continue
		}
		prefix := r.DestinationPrefix.Prefix()
		if prefix.Addr().IsUnspecified() && prefix.Bits() == 0 {
			if !found || r.Metric < bestMetric {
				found = true
				bestMetric = r.Metric
				bestRow = *r
			}
		}
	}
	if !found {
		return 0, netip.Addr{}, errors.New("no IPv4 default route found")
	}
	return bestRow.InterfaceIndex, bestRow.NextHop.Addr(), nil
}

// guidString renders a windows.GUID in the braced form the device URL
// query expects.
func guidString(g windows.GUID) string {
	return fmt.Sprintf("{%s}", g.String())
}

// startWatchdog guards the user's network against a dead pipeline: if the
// self-probe (an HTTPS fetch routed through the whole TUN pipeline) does
// not succeed shortly after enable — or stops succeeding later — the TUN
// is torn down automatically and traffic falls back to the physical
// adapter. Without this a broken forwarding path would black-hole every
// connection on the machine until manual cleanup.
func (m *tunManager) startWatchdog(params TunParams) {
	go func() {

		rollback := func(reason string) {
			loggo.Error("[TUN] watchdog rollback: %s", reason)
			_ = m.disable()
			if params.OnRollback != nil {
				params.OnRollback(reason)
			}
		}

		// Soak: give the stack a moment, then demand a working pipeline.
		// Default soak probes at ~3s and ~6s; a params.Soak deadline keeps
		// the adapter up longer (diagnostics) before rolling back.
		deadline := 6 * time.Second
		interval := 3 * time.Second
		if params.Soak > 0 {
			deadline = params.Soak
			interval = 5 * time.Second
		}
		loggo.Info("[TUN] watchdog: soak=%v deadline=%v interval=%v", params.Soak, deadline, interval)
		soakStart := time.Now()
		var lastErr error
		for time.Since(soakStart) < deadline {
			time.Sleep(interval)
			if m.stopped.Load() {
				return
			}
			if err := pipelineProbe(); err == nil {
				lastErr = nil
				break
			} else {
				lastErr = err
				loggo.Warn("[TUN] watchdog soak probe failed: %v", err)
			}
		}
		if lastErr != nil {
			rollback(fmt.Sprintf("self-probe failed: %v", lastErr))
			return
		}
		loggo.Info("[TUN] watchdog: pipeline verified, probing every 15s")

		failures := 0
		for {
			time.Sleep(15 * time.Second)
			if m.stopped.Load() {
				return
			}
			if err := pipelineProbe(); err != nil {
				failures++
				if failures >= 3 {
					rollback(fmt.Sprintf("pipeline dead: %v", err))
					return
				}
			} else {
				failures = 0
			}
		}
	}()
}

// pipelineProbe fetches probeURL with no proxy: the request crosses TUN
// capture, the DNS hijack, the handler, the engine inbound and a bound
// direct dial — exactly the path a real user request takes.
func pipelineProbe() error {
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(probeURL)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// ensureWintunDLL puts the embedded wintun.dll where the standalone wintun
// module's LoadLibraryEx finds it: next to the exe (application directory)
// or, when that directory is read-only, in System32 (we are elevated when
// TUN runs).
func ensureWintunDLL() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exeDir := filepath.Dir(exe)
	dst := filepath.Join(exeDir, "wintun.dll")
	if wintunDLLPresent(dst) {
		return nil
	}
	if err := os.WriteFile(dst, wintunDLL, 0o644); err == nil {
		loggo.Info("[TUN] extracted wintun.dll to %s", dst)
		return nil
	}
	sys32 := filepath.Join(os.Getenv("SystemRoot"), "System32", "wintun.dll")
	if wintunDLLPresent(sys32) {
		return nil
	}
	if err := os.WriteFile(sys32, wintunDLL, 0o644); err != nil {
		return fmt.Errorf("write wintun.dll to %s and %s: %w", dst, sys32, err)
	}
	loggo.Info("[TUN] extracted wintun.dll to %s", sys32)
	return nil
}

// wintunDLLPresent checks size, not just existence: a truncated copy from
// a crashed write would make every later LoadLibraryEx fail confusingly.
func wintunDLLPresent(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Size() == int64(len(wintunDLL))
}
