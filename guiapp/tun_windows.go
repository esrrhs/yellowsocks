//go:build windows

package guiapp

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
	appdns "github.com/esrrhs/yellowsocks/core/dns"
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

// probeProxyHost is a name that is always routed through the upstream. Its
// answer must come back from the fake-ip pool; a real address means the DNS
// hijack is not in effect.
const probeProxyHost = "www.google.com"

// fakeIPNet is the pool the DNS layer hands out, used to recognise a fake
// answer without duplicating the range.
var fakeIPNet = func() *net.IPNet {
	_, n, err := net.ParseCIDR(appdns.DefaultFakeIPRange)
	if err != nil {
		panic("invalid DefaultFakeIPRange: " + err.Error())
	}
	return n
}()

//go:embed tun/wintun_amd64.dll
var wintunDLL []byte

// The adapter is created by tun2socks (plain "tun://YellowSocks"), so its
// device GUID is the library's; what matters here is that the SwDevice is
// bound to the creating process — when that process dies, the device and its
// takeover routes are removed with it, which is the last-resort recovery from
// a crash that never got to run teardown.

// tunBringUpDeadline bounds how long a single enable() may keep the OS in a
// half-configured state before the bring-up guard force-rolls it back. The
// normal bring-up takes a few seconds (UAC plus adapter creation); 60s only
// ever triggers on a hung wintun call, a wedged tun2socks start or a stuck
// route transaction — the cases where a deadlock would otherwise leave the
// machine routed into a pipeline that never comes up.
const tunBringUpDeadline = 60 * time.Second

// tunManager owns the tun2socks engine lifecycle and the Windows routing
// state around it.
type tunManager struct {
	mu         sync.Mutex
	active     atomic.Bool    // lock-free: the guard and disable() read it
	bringingUp atomic.Bool    // true between "touch the OS" and "verified"
	hung       atomic.Bool    // latched by forceTeardown; later calls skip mu
	stopped    atomic.Bool    // stops the pipeline watchdog
	stopMu     sync.Mutex     // serializes tun2socks Stop calls
	openHandle *wintun.Adapter // keeps our OpenAdapter reference alive
	luid       wgwinipcfg.LUID
	physIfIdx  uint32
	physLUID   wgwinipcfg.LUID
	physGw     netip.Addr
	upstreamRT []netip.Prefix // /32 routes for upstream servers on the physical adapter
	// stackUp records that the tun2socks stack was started in this process.
	// It is never stopped until exit (see enable()): the library cannot be
	// restarted, and its failure path exits the process.
	stackUp    atomic.Bool
	stackProxy string // upstream the running stack forwards into
	// dnsBackup holds the resolver lists replaced for the take-over. It is
	// published atomically as well so forceTeardown() can put them back
	// without taking mu (which a stuck enable() may hold forever).
	dnsBackup atomic.Value // []dnsOverride
	// rollbackFn is the OnRollback callback of the current run, published so
	// the forced teardown can report why the adapter disappeared.
	rollbackFn atomic.Value // func(string)
}

// dnsOverride remembers one interface's resolver list so it can be put back
// when the TUN goes down.
type dnsOverride struct {
	luid    wgwinipcfg.LUID
	alias   string // friendly name, used for the crash-recovery journal
	servers []netip.Addr
}

var manager = &tunManager{}

func tunEnable(params TunParams) error { return manager.enable(params) }
func tunShutdownPlatform() error      { manager.shutdown(); return nil }
func tunDisable() error                { return manager.disable() }
func tunIsActive() bool                { return manager.isActive() }

// tunSessionStatsLive returns live TUN diagnostics for the driver/GUI. It
// never blocks on mu: a stuck bring-up must not freeze the diagnostics the
// watchdog and the GUI poll.
func tunSessionStatsLive() string {
	m := manager
	if !m.active.Load() {
		return "inactive"
	}
	if m.hung.Load() {
		return "hung"
	}
	if m.mu.TryLock() {
		defer m.mu.Unlock()
		return fmt.Sprintf("physIfIdx=%d upstreamExceptions=%d", m.physIfIdx, len(m.upstreamRT))
	}
	return "busy"
}

func (m *tunManager) isActive() bool {
	return m.active.Load()
}

// lockOrForce takes m.mu, giving up when a bring-up or teardown is stuck on
// it. A hung enable() holds the lock forever, and every later disable would
// block on it — the user could then never switch TUN off, and the watchdog
// could never roll back. Returning false tells the caller to unwind with the
// lock-free path instead.
func (m *tunManager) lockOrForce(what string) bool {
	deadline := time.Now().Add(3 * time.Second)
	for {
		if m.mu.TryLock() {
			return true
		}
		if time.Now().After(deadline) {
			loggo.Error("[TUN] %s: state lock still held after 3s (a bring-up or teardown is stuck)", what)
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (m *tunManager) enable(params TunParams) (err error) {
	if !m.lockOrForce("enable") {
		m.forceTeardown("state lock unavailable before enable (a previous bring-up is stuck)")
		return errors.New("TUN state is locked by a stuck bring-up; forced cleanup requested")
	}
	defer m.mu.Unlock()
	if m.active.Load() {
		return errors.New("TUN mode already active")
	}
	m.stopped.Store(false)
	m.hung.Store(false)
	m.bringingUp.Store(true)
	// atomic.Value panics on a nil store, and the callback is published so
	// the forced teardown can report a rollback it did not initiate.
	rollback := params.OnRollback
	if rollback == nil {
		rollback = func(string) {}
	}
	m.rollbackFn.Store(rollback)

	// Bring-up guard: a hung wintun/tun2socks call must never leave the
	// machine routed into a half-built adapter. This goroutine does not take
	// m.mu — if enable() deadlocks while holding it, the guard is the only
	// thing that can still unwind the routing state. It is disarmed as soon
	// as enable() returns (success or failure).
	guardStop := make(chan struct{})
	guardDone := make(chan struct{})
	go func() {
		defer close(guardDone)
		select {
		case <-time.After(tunBringUpDeadline):
			if m.bringingUp.Load() {
				m.forceTeardown(fmt.Sprintf("bring-up did not finish within %v", tunBringUpDeadline))
			}
		case <-guardStop:
		}
	}()
	// Registered after the unlock defer, so it runs BEFORE the lock is
	// released: the guard is never left armed against a finished bring-up.
	defer func() {
		m.bringingUp.Store(false)
		close(guardStop)
		<-guardDone
	}()
	// A panic in the middle of the OS setup would otherwise leave the
	// adapter up with the takeover routes installed.
	defer func() {
		if r := recover(); r != nil {
			m.forceTeardown(fmt.Sprintf("panic during bring-up: %v", r))
			err = fmt.Errorf("TUN bring-up panicked: %v", r)
		}
	}()
	var perr error

	// 1. wintun.dll on disk: the standalone wintun module loads it with
	// LoadLibraryEx from the application directory (or System32).
	if err := ensureWintunDLL(); err != nil {
		return fmt.Errorf("prepare wintun.dll: %w", err)
	}

	// 2. The tun2socks engine creates the wintun adapter itself, the gvisor
	// stack, and forwards every captured flow into the engine's SOCKS5
	// inbound — the same proxy Clash Verge's kernel forwards into.
	//
	// The stack is started AT MOST ONCE per process. tun2socks v2 keeps its
	// device/stack in package globals: Stop() closes them but leaves the
	// globals set, and a later Start() runs against that torn-down state —
	// on failure it calls log.Fatalf, which exits the process. Turning TUN
	// off and on again would therefore kill the GUI (silently: the fatal
	// line goes to a console that dies with the process). Keeping the stack
	// alive and treating "TUN off" as "remove the capture point" (the
	// adapter address and the takeover routes) is what Clash does too, and
	// it makes toggling instant instead of a full adapter rebuild.
	if !m.stackUp.Load() {
		tun2socks_engine.Insert(&tun2socks_engine.Key{
			Device:     "tun://" + tunAdapterName,
			Proxy:      "socks5://" + params.Socks5Addr,
			LogLevel:   "debug",
			MTU:        tunMTU,
			UDPTimeout: 60 * time.Second,
		})
		tun2socks_engine.Start()
		m.stackUp.Store(true)
		m.stackProxy = params.Socks5Addr
	} else if m.stackProxy != params.Socks5Addr {
		// The captured flows must reach the engine's inbound on the address
		// it listens on now. Re-pointing the stack means restarting it,
		// which cannot be done safely in-process, so refuse instead of
		// risking an exit or a silently dead pipeline.
		return fmt.Errorf("TUN 引擎已在运行且上游 SOCKS5 端口已改变 (%s → %s)；请退出应用后重新开启 TUN",
			m.stackProxy, params.Socks5Addr)
	}

	// 3. The adapter now exists: grab its LUID for the routing config.
	// OpenAdapter keeps a second handle open for the lifetime of the
	// process; NEVER close it (WintunCloseAdapter removes the adapter
	// instance, and the next enable would have nothing to route into). It is
	// released only when the app exits, after which the process-bound
	// SwDevice is removed too, self-healing leftovers.
	adapterDev := m.openHandle
	if adapterDev == nil {
		opened, oerr := openAdapterRetry(tunAdapterName, 5*time.Second)
		if oerr != nil {
			m.teardownLocked()
			return fmt.Errorf("open TUN adapter: %w", oerr)
		}
		adapterDev = opened
		m.openHandle = adapterDev
	}
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
	// Undo any take-over a previous run left behind (crash, power loss)
	// before installing this run's, so the journal holds the real originals.
	recoverDNSState()
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
	//
	// An "already exists" answer is not success: it means a previous run (or
	// another adapter now gone) left a route for this prefix, and if it points
	// at a dead interface the proxy's own tunnel would be black-holed. Replace
	// it so the route is this run's — and therefore gets removed on teardown.
	for _, ipStr := range params.UpstreamIPs {
		ip := netip.MustParseAddr(ipStr)
		prefix := netip.PrefixFrom(ip, ip.BitLen())
		if err := m.physLUID.AddRoute(prefix, m.physGw, 1); err != nil {
			// Delete is best effort: the stale route may live on an
			// interface that no longer exists.
			_ = m.physLUID.DeleteRoute(prefix, m.physGw)
			if derr := m.physLUID.AddRoute(prefix, m.physGw, 1); derr != nil {
				loggo.Warn("[TUN] upstream /32 route %s via %s: %v (after replacing a stale route: %v)",
					prefix, m.physGw, derr, err)
				continue
			}
			loggo.Info("[TUN] upstream /32 route %s replaced a stale entry", prefix)
		}
		m.upstreamRT = append(m.upstreamRT, prefix)
	}

	m.active.Store(true)
	m.bringingUp.Store(false)
	loggo.Info("[TUN] adapter %s up (%s/30, MTU %d); physical iface #%d via %s; upstream exceptions: %d",
		tunAdapterName, tunIPv4, tunMTU, m.physIfIdx, m.physGw, len(m.upstreamRT))
	m.startWatchdog(params)
	return nil
}

func (m *tunManager) disable() error {
	if !m.lockOrForce("disable") {
		// enable() (or a previous teardown) is stuck holding the lock. The
		// only way to get the user's network back is the lock-free path.
		m.forceTeardown("state lock unavailable during disable")
		return nil
	}
	defer m.mu.Unlock()
	if !m.active.Load() {
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

	// 2. The tun2socks stack and our adapter handle stay up on purpose: the
	// library is single-shot per process (see enable), and with no address
	// and no route on the adapter it captures nothing, so the machine is
	// back on its own default route. The next enable reuses both, which is
	// also why toggling is fast.
	m.active.Store(false)
}

// stopStack shuts tun2socks down for good. Only valid on the way out of the
// process: the library cannot be started again afterwards.
func (m *tunManager) stopStack() {
	if !m.stackUp.CompareAndSwap(true, false) {
		return
	}
	if m.stopMu.TryLock() {
		tun2socks_engine.Stop()
		m.stopMu.Unlock()
	}
}

// forceTeardown unwinds the TUN state WITHOUT taking m.mu.
//
// It is the last line of defence: enable() holds mu for the whole bring-up,
// so if a wintun/tun2socks call deadlocks inside it, both the pipeline
// watchdog's rollback and a user toggle would block on that lock forever —
// leaving the machine routed into a pipeline that never answers. Everything
// here reads only atomics or re-discovers state from the OS, and every step
// is best effort, so it makes progress (and gives the network back) even in
// that state.
func (m *tunManager) forceTeardown(reason string) {
	if m.stopped.Swap(true) && !m.active.Load() && !m.bringingUp.Load() {
		return // already unwound and nothing was up
	}
	m.hung.Store(true)
	loggo.Error("[TUN] FORCED teardown: %s", reason)

	// 1. Routing state first: find our adapter by name (the LUID may never
	// have been stored if the bring-up died early) and drop its takeover
	// routes, address and resolver. With no /1 route left, traffic falls
	// back to the physical default route even if the adapter lingers.
	family := wgwinipcfg.AddressFamily(windows.AF_INET)
	if luid, ok := luidByName(tunAdapterName); ok {
		_ = luid.FlushRoutes(family)
		_ = luid.FlushIPAddresses(family)
		_ = luid.FlushDNS(family)
	}
	// 2. Resolvers back on every interface we redirected, and the
	// multi-homed policy released, before anything else can fail.
	m.restorePhysicalDNS()
	if err := setSmartResolutionDisabled(false); err != nil {
		loggo.Warn("[TUN] forced teardown: restore smart name resolution: %v", err)
	}
	_, _ = exec.Command("ipconfig", "/flushdns").CombinedOutput()

	// 3. The tun2socks stack is deliberately left running: with the adapter's
	// address and routes gone it captures nothing, and stopping it would be
	// both unnecessary and unsafe (single-shot library). Everything that can
	// block is above; this path never waits on the stack.

	m.active.Store(false)
	m.bringingUp.Store(false)

	// 4. Tell the UI why the adapter vanished so the user is not left with a
	// silent "TUN on" indicator and no connectivity.
	if fn, ok := m.rollbackFn.Load().(func(string)); ok && fn != nil {
		go fn("TUN 已强制回滚: " + reason)
	}
}

// luidByName resolves an adapter's LUID from its friendly name. Used by the
// forced teardown, which must not depend on state the bring-up may never
// have recorded.
func luidByName(name string) (wgwinipcfg.LUID, bool) {
	adapters, err := wgwinipcfg.GetAdaptersAddresses(windows.AF_INET, wgwinipcfg.GAAFlagIncludeAll)
	if err != nil {
		loggo.Warn("[TUN] enumerate adapters: %v", err)
		return 0, false
	}
	for _, a := range adapters {
		if a.FriendlyName() == name {
			return a.LUID, true
		}
	}
	return 0, false
}

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
	var backup []dnsOverride
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
		alias := tunAliasByLUID(row.InterfaceLUID)
		if len(prev) == 1 && prev[0] == target {
			continue // already pointed here by an earlier run
		}
		if serr := row.InterfaceLUID.SetDNS(family, []netip.Addr{target}, nil); serr != nil {
			loggo.Warn("[TUN] point interface %d at %s: %v", row.InterfaceIndex, tunProbeDNS, serr)
			continue
		}
		backup = append(backup, dnsOverride{luid: row.InterfaceLUID, alias: alias, servers: prev})
	}
	// Journal first, then apply: a crash between the two leaves a file the
	// next bring-up repairs, while a crash after both leaves the resolvers
	// recorded. Publishing comes before the take-over is reported so a forced
	// teardown racing this point still sees the list it has to put back.
	entries := make([]tunStateEntry, 0, len(backup))
	for _, b := range backup {
		e := tunStateEntry{Alias: b.alias}
		for _, srv := range b.servers {
			e.Servers = append(e.Servers, srv.String())
		}
		entries = append(entries, e)
	}
	saveDNSState(entries)
	m.dnsBackup.Store(backup)
	loggo.Info("[TUN] DNS take-over: %d interface(s) now resolve through %s", len(backup), tunProbeDNS)
}

// dnsOverrideSnapshot returns the resolver lists replaced by the take-over.
func (m *tunManager) dnsOverrideSnapshot() []dnsOverride {
	if v := m.dnsBackup.Load(); v != nil {
		if list, ok := v.([]dnsOverride); ok {
			return list
		}
	}
	return nil
}

// restorePhysicalDNS puts every resolver list saved by overridePhysicalDNS
// back. Best effort: each interface is restored independently, and the saved
// list is cleared first so a concurrent path cannot restore twice.
func (m *tunManager) restorePhysicalDNS() {
	saved := m.dnsOverrideSnapshot()
	if len(saved) == 0 {
		return
	}
	m.dnsBackup.Store([]dnsOverride(nil))
	family := wgwinipcfg.AddressFamily(windows.AF_INET)
	for _, b := range saved {
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
	clearDNSState()
	loggo.Info("[TUN] DNS take-over released: %d interface(s) restored", len(saved))
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

		// The international path is monitored separately: a broken upstream
		// means "外网不通" while the machine is still online, which is the
		// failure users actually report. It is reported, never rolled back —
		// tearing the adapter down would not bring the upstream back, and it
		// would take the working domestic path with it.
		if err := proxiedProbe(); err != nil {
			loggo.Warn("[TUN] watchdog: proxied path not working yet: %v", err)
		}

		failures := 0
		proxyFailures := 0
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
				if err := proxiedProbe(); err != nil {
					proxyFailures++
					if proxyFailures%4 == 1 {
						loggo.Warn("[TUN] proxied path failing (domestic still fine): %v", err)
					}
				} else {
					proxyFailures = 0
				}
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

// proxiedProbe walks the foreign path: resolve a proxied name (which must
// come back from the fake-ip pool, proving the DNS hijack is in effect) and
// connect to the fake address, which the engine translates back to the domain
// and forwards to the upstream. No HTTP: the TLS payload is not what is under
// test, and a 443 connect is the cheapest proof the relay works.
func proxiedProbe() error {
	ips, err := net.LookupIP(probeProxyHost)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", probeProxyHost, err)
	}
	var fake net.IP
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil && fakeIPNet.Contains(v4) {
			fake = v4
			break
		}
	}
	if fake == nil {
		return fmt.Errorf("%s did not resolve to a fake address (%v): DNS hijack not in effect", probeProxyHost, ips)
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(fake.String(), "443"), 6*time.Second)
	if err != nil {
		return fmt.Errorf("connect to fake %s: %w", fake, err)
	}
	_ = conn.Close()
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

// openAdapterRetry opens the wintun adapter, retrying briefly: right after the
// engine creates it the device node can take a moment to become openable.
func openAdapterRetry(name string, timeout time.Duration) (*wintun.Adapter, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		a, err := wintun.OpenAdapter(name)
		if err == nil {
			return a, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// shutdown releases the TUN for good: routes and address come off, then the
// tun2socks stack (and with it the adapter) is stopped. Only App.Shutdown
// calls this — the stack cannot be started again afterwards.
func (m *tunManager) shutdown() {
	if m.lockOrForce("shutdown") {
		if m.active.Load() {
			m.teardownLocked()
		}
		m.mu.Unlock()
	} else {
		m.forceTeardown("shutdown with the state lock held")
	}
	m.stopStack()
	if m.openHandle != nil {
		_ = m.openHandle.Close()
		m.openHandle = nil
	}
}

// tunStateName is the crash-recovery journal for the DNS take-over. It is
// written BEFORE an interface's resolvers are replaced and removed after they
// are put back; a leftover file therefore means the previous run died while
// the take-over was active and its original servers still have to be
// restored. Without it, a crash leaves every adapter pointing at our resolver
// and nobody remembers what DHCP handed out.
const tunStateName = "tun-dns-state.json"

type tunStateEntry struct {
	Alias   string   `json:"alias"`
	Servers []string `json:"servers"`
}

type tunState struct {
	DNS []tunStateEntry `json:"dns"`
}

// tunStateDir is where the journal is written: the app's data directory in
// production, a temp directory under test.
var tunStateDir = DataDir

func tunStatePath() string { return filepath.Join(tunStateDir(), tunStateName) }

// saveDNSState persists the resolver lists the take-over is about to replace.
func saveDNSState(entries []tunStateEntry) {
	if len(entries) == 0 {
		return
	}
	data, err := json.MarshalIndent(tunState{DNS: entries}, "", "  ")
	if err != nil {
		loggo.Warn("[TUN] encode DNS recovery state: %v", err)
		return
	}
	if err := os.WriteFile(tunStatePath(), data, 0o644); err != nil {
		loggo.Warn("[TUN] write DNS recovery state: %v", err)
	}
}

// clearDNSState drops the journal once the resolvers are back.
func clearDNSState() {
	if err := os.Remove(tunStatePath()); err != nil && !os.IsNotExist(err) {
		loggo.Warn("[TUN] remove DNS recovery state: %v", err)
	}
}

// loadDNSState reads the journal left behind by a crashed run. A corrupt or
// empty journal is dropped rather than kept: it can never be applied, and
// leaving it in place would mean warning on every later bring-up.
func loadDNSState() ([]tunStateEntry, bool) {
	data, err := os.ReadFile(tunStatePath())
	if err != nil {
		return nil, false
	}
	var st tunState
	if err := json.Unmarshal(data, &st); err != nil {
		loggo.Warn("[TUN] parse DNS recovery state: %v", err)
		clearDNSState()
		return nil, false
	}
	if len(st.DNS) == 0 {
		clearDNSState()
		return nil, false
	}
	return st.DNS, true
}

// recoverDNSState undoes a take-over left behind by a crashed run. It runs at
// the start of enable(), before this run installs its own (and journals its
// own) resolvers, so the aliases and server lists are the previous run's
// originals rather than our own 223.5.5.5.
func recoverDNSState() {
	entries, ok := loadDNSState()
	if !ok {
		return
	}
	family := wgwinipcfg.AddressFamily(windows.AF_INET)
	restored := 0
	for _, e := range entries {
		luid, ok := luidByFriendlyName(e.Alias)
		if !ok {
			continue
		}
		var servers []netip.Addr
		for _, s := range e.Servers {
			if addr, err := netip.ParseAddr(s); err == nil {
				servers = append(servers, addr)
			}
		}
		if len(servers) == 0 {
			servers = nil // DHCP-provided originally
		}
		if err := luid.SetDNS(family, servers, nil); err != nil {
			loggo.Warn("[TUN] recover resolvers on %s: %v", e.Alias, err)
			continue
		}
		restored++
	}
	clearDNSState()
	if restored > 0 {
		loggo.Info("[TUN] recovered %d adapter(s) from a previous run that died with the DNS take-over active", restored)
		_, _ = exec.Command("ipconfig", "/flushdns").CombinedOutput()
	}
}

// luidByFriendlyName resolves an adapter LUID from the alias stored in the
// recovery journal.
func luidByFriendlyName(alias string) (wgwinipcfg.LUID, bool) {
	adapters, err := wgwinipcfg.GetAdaptersAddresses(windows.AF_INET, wgwinipcfg.GAAFlagIncludeAll)
	if err != nil {
		return 0, false
	}
	for _, a := range adapters {
		if a.FriendlyName() == alias {
			return a.LUID, true
		}
	}
	return 0, false
}

// tunAliasByLUID returns an interface's friendly name (empty when unknown),
// used to key the crash-recovery journal by a name that survives reboots.
func tunAliasByLUID(luid wgwinipcfg.LUID) string {
	adapters, err := wgwinipcfg.GetAdaptersAddresses(windows.AF_INET, wgwinipcfg.GAAFlagIncludeAll)
	if err != nil {
		return ""
	}
	for _, a := range adapters {
		if a.LUID == luid {
			return a.FriendlyName()
		}
	}
	return ""
}
