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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/yellowsocks/core/netdial"
	tun2socks_engine "github.com/xjasonlyu/tun2socks/v2/engine"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
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
	dnsBackup  []dnsOverride  // interface DNS servers replaced for the take-over
}

// dnsOverride remembers one interface's resolver list so it can be put back
// when the TUN goes down.
type dnsOverride struct {
	luid    wgwinipcfg.LUID
	servers []netip.Addr
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

	// 5. Interface tuning (mirrors wireguard-windows / sing-tun): router
	// discovery and DAD off (the address must be usable instantly, not sit
	// tentative while traffic black-holes), fixed MTU, and an interface
	// metric of 0 so the takeover routes win immediately.
	//
	// IP forwarding MUST stay off. With forwarding enabled tcpip.sys treats
	// the machine as a router for packets arriving on this interface: an
	// inbound SYN-ACK is not delivered to the local TCP stack, Windows emits
	// an ICMP redirect ("198.18.0.1 to net 198.18.0.1") and resets the
	// connection. pktmon shows the exact sequence — SYN, a correct
	// SYN-ACK, ICMP redirect, RST — and tun2socks consequently reports
	// "connection was refused" for every TCP forward while UDP (which has no
	// handshake to lose) keeps working.
	ipif, err := tunLUID.IPInterface(wgwinipcfg.AddressFamily(windows.AF_INET))
	if err != nil {
		m.teardownLocked()
		return fmt.Errorf("read IP interface: %w", err)
	}
	ipif.ForwardingEnabled = false
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

	// 6. Default-route takeover as two half-routes (0.0.0.0/1 + 128.0.0.0/1)
	// so the physical default route stays in place as the fallback when the
	// TUN goes down.
	//
	// NextHop is the zero address = ON-LINK, which is mandatory for wintun:
	// it is a pure L3 adapter (NdisMediumIP) that cannot resolve neighbours
	// (New-NetNeighbor -LinkLayerAddress fails with "does not support
	// setting LinkLayerAddress").
	//
	// On-link routes (NextHop 0.0.0.0) are NOT usable here either: they turn
	// every destination into a directly-connected neighbour, Windows starts
	// neighbour unreachability detection for each of them, and since an L3
	// adapter can never answer, popular destinations end up "Unreachable" in
	// the neighbour table — traffic then dies a few seconds after enable.
	//
	// The working combination is a gateway next hop plus a PERMANENT
	// neighbour entry for that gateway that carries no link-layer address:
	// Windows keeps the entry reachable, so it never probes, and the packets
	// are handed to wintun, which needs no L2 address at all.
	gateway := netip.MustParseAddr(tunGateway4)
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
	// Permanent gateway neighbour, best effort: Windows rejects the
	// LinkLayerAddress form on this media type, but accepts the entry without
	// one, which is all the stack needs to stop probing.
	ps := fmt.Sprintf("New-NetNeighbor -InterfaceAlias '%s' -IPAddress %s -State Permanent -PolicyStore ActiveStore -ErrorAction Stop",
		tunAdapterName, tunGateway4)
	if output, nerr := exec.Command("powershell", "-NoProfile", "-Command", ps).CombinedOutput(); nerr != nil {
		loggo.Warn("[TUN] permanent gateway neighbour %s: %v: %s", tunGateway4, nerr, string(output))
	}
	// SetRoutesForFamily would fail with ERROR_NOT_FOUND on a freshly
	// created adapter (its internal flush finds no route rows yet): flush
	// tolerantly, then plain-add the takeover routes.
	_ = tunLUID.FlushRoutes(wgwinipcfg.AddressFamily(windows.AF_INET))
	if err := tunLUID.AddRoutes(routesFor()); err != nil {
		m.teardownLocked()
		return fmt.Errorf("add v4 routes: %w", err)
	}

	// 7. Adapter DNS: the engine's in-process :53 hijack, so every resolver
	// query on the machine is answered by the smart resolver and proxied
	// domains receive a fake-ip from the pool.
	//
	// The address itself is irrelevant (the datagram is hijacked before it
	// reaches the wire), but Windows only sends DNS to the adapter when the
	// adapter owns a server entry, so one must be published. Do NOT call
	// wgwinipcfg's LUID.FlushDNS here: it is implemented as
	// SetDNS(family, nil, nil), which CLEARS the very servers just set —
	// that silently left the adapter with an empty DNS list, Windows kept
	// resolving through the physical NIC and every proxied name received a
	// poisoned answer instead of a fake-ip. Resolver-cache invalidation is a
	// separate concern and is done through ipconfig /flushdns below.
	if err := tunLUID.SetDNS(
		wgwinipcfg.AddressFamily(windows.AF_INET),
		[]netip.Addr{netip.MustParseAddr(tunProbeDNS)}, []string{},
	); err != nil {
		m.teardownLocked()
		return fmt.Errorf("set TUN DNS: %w", err)
	}
	// The cache still holds answers learned before the takeover (polluted
	// real IPs for proxied names): without this flush the first lookups of
	// every domain return those stale records and skip fake-ip entirely.
	if output, ferr := exec.Command("ipconfig", "/flushdns").CombinedOutput(); ferr != nil {
		loggo.Warn("[TUN] flush resolver cache: %v: %s", ferr, string(output))
	}
	// Windows answers a lookup by asking EVERY interface's DNS servers in
	// parallel and taking the first reply ("smart multi-homed name
	// resolution"). The physical NIC's resolver answers a polluted record in
	// milliseconds, so without turning that behaviour off the TUN's fake-ip
	// answer loses the race and proxied names keep resolving to blocked real
	// addresses. This is the same policy Clash/sing-box apply on Windows.
	if err := setSmartResolutionDisabled(true); err != nil {
		loggo.Warn("[TUN] disable smart name resolution: %v", err)
	}
	// The DNS Client service reads that policy at start, so it has to be
	// restarted before the policy has any effect on lookups; flushing alone
	// leaves the old behaviour (ask every adapter, first polluted answer
	// wins) in place. Costs a fraction of a second of resolution during
	// bring-up, which is invisible next to the UAC/adapter setup around it.
	if output, rerr := exec.Command("powershell", "-NoProfile", "-Command",
		"Restart-Service -Name dnscache -Force -ErrorAction Stop").CombinedOutput(); rerr != nil {
		loggo.Warn("[TUN] restart DNS client to apply resolver policy: %v: %s", rerr, strings.TrimSpace(string(output)))
	}
	if output, ferr := exec.Command("ipconfig", "/flushdns").CombinedOutput(); ferr != nil {
		loggo.Warn("[TUN] flush resolver cache after policy: %v: %s", ferr, string(output))
	}
	if servers, serr := tunLUID.DNS(); serr != nil {
		loggo.Warn("[TUN] read back TUN DNS: %v", serr)
	} else if len(servers) == 0 {
		loggo.Warn("[TUN] TUN adapter has no IPv4 DNS server: system lookups will bypass the hijack")
	}
	m.overridePhysicalDNS(tunLUID)

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
	// Put the interfaces' own resolvers back before the policy is released:
	// with the TUN gone the physical adapters must answer lookups again.
	m.restorePhysicalDNS()
	// Restore the resolver policy: with the TUN gone the physical adapters
	// must answer lookups again, and a stale policy would keep them from
	// being consulted.
	if err := setSmartResolutionDisabled(false); err != nil {
		loggo.Warn("[TUN] restore smart name resolution: %v", err)
	}
	_, _ = exec.Command("ipconfig", "/flushdns").CombinedOutput()
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

// dnsClientPolicyKey is where Windows keeps the resolver policies Clash and
// sing-box flip while a TUN is up.
const dnsClientPolicyKey = `SOFTWARE\Policies\Microsoft\Windows NT\DNSClient`

// setSmartResolutionDisabled turns Windows' multi-homed name resolution off
// (disabled=true) so lookups go to the TUN's hijacking resolver only, and
// removes the policy again on teardown. Deleting the values restores the
// default behaviour instead of pinning a policy the user never asked for.
func setSmartResolutionDisabled(disabled bool) error {
	key, _, err := registry.CreateKey(registry.LOCAL_MACHINE, dnsClientPolicyKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	for _, name := range []string{"DisableSmartNameResolution", "DisableSmartMultiHomedNameResolution"} {
		if !disabled {
			if derr := key.DeleteValue(name); derr != nil && !errors.Is(derr, registry.ErrNotExist) {
				return derr
			}
			continue
		}
		if serr := key.SetDWordValue(name, 1); serr != nil {
			return serr
		}
	}
	return nil
}

// overridePhysicalDNS points every other IPv4 interface at the TUN's
// resolver for the lifetime of the adapter.
//
// Publishing 223.5.5.5 on the TUN alone is not enough: Windows still answers
// lookups from whichever interface it likes, and the physical NIC's resolver
// returns a polluted record in single-digit milliseconds — long before the
// TUN's fake-ip answer comes back. The proxy decision is made from the
// domain, so a polluted REAL address both skips fake-ip and points the
// connection at a blocked IP. Redirecting every interface's resolver at the
// hijacked address makes the outcome independent of Windows' interface
// choice.
//
// 223.5.5.5 is a public resolver, so even a missed restore (crash, power
// loss) leaves the machine with working DNS; the normal path puts the
// original servers back on teardown.
func (m *tunManager) overridePhysicalDNS(tunLUID wgwinipcfg.LUID) {
	rows, err := wgwinipcfg.GetIPInterfaceTable(wgwinipcfg.AddressFamily(windows.AF_INET))
	if err != nil {
		loggo.Warn("[TUN] enumerate interfaces for DNS take-over: %v", err)
		return
	}
	target := netip.MustParseAddr(tunProbeDNS)
	family := wgwinipcfg.AddressFamily(windows.AF_INET)
	for i := range rows {
		row := &rows[i]
		// Loopback (index 1) and our own adapter keep their settings.
		if row.InterfaceLUID == tunLUID || row.InterfaceIndex == 1 {
			continue
		}
		prev, derr := row.InterfaceLUID.DNS()
		if derr != nil {
			continue
		}
		if len(prev) == 1 && prev[0] == target {
			continue // already pointed here by an earlier run
		}
		if serr := row.InterfaceLUID.SetDNS(family, []netip.Addr{target}, nil); serr != nil {
			loggo.Warn("[TUN] point interface %d at %s: %v", row.InterfaceIndex, tunProbeDNS, serr)
			continue
		}
		m.dnsBackup = append(m.dnsBackup, dnsOverride{luid: row.InterfaceLUID, servers: prev})
	}
	loggo.Info("[TUN] DNS take-over: %d interface(s) now resolve through %s", len(m.dnsBackup), tunProbeDNS)
}

// restorePhysicalDNS puts every resolver list saved by overridePhysicalDNS
// back. Best effort: each interface is restored independently.
func (m *tunManager) restorePhysicalDNS() {
	if len(m.dnsBackup) == 0 {
		return
	}
	family := wgwinipcfg.AddressFamily(windows.AF_INET)
	for _, b := range m.dnsBackup {
		servers := b.servers
		if len(servers) == 0 {
			// The interface had no explicit servers (DHCP-provided): an
			// empty list hands it back to DHCP.
			servers = nil
		}
		if err := b.luid.SetDNS(family, servers, nil); err != nil {
			loggo.Warn("[TUN] restore interface DNS: %v", err)
		}
	}
	loggo.Info("[TUN] DNS take-over released: %d interface(s) restored", len(m.dnsBackup))
	m.dnsBackup = nil
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
