package sppclient

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/yellowsocks/core/netdial"
)

// Node is one upstream SPP channel/server configuration.
type Node struct {
	Name        string `json:"name" yaml:"name"`
	// Type selects what this node dials: "" / "spp" (default) is an SPP
	// channel with the fields below, "socks5" is a plain RFC 1928 SOCKS5
	// server reached directly (server + port + username/password). Both
	// kinds live in the same priority pool: a socks5 backup can sit at
	// priority 2 behind an SPP channel at 1.
	Type         string `json:"type,omitempty" yaml:"type,omitempty"`
	Server       string `json:"server" yaml:"server"`
	// Port is only meaningful for socks5 nodes, whose config splits host and
	// port the way every socks5 client does.
	Port         int    `json:"port,omitempty" yaml:"port,omitempty"`
	ServerProto  string `json:"server_proto" yaml:"server_proto"` // tcp, udp, rudp, kcp, quic, pingtunnel, rdns
	Key          string `json:"key" yaml:"key"`
	Username     string `json:"username,omitempty" yaml:"username,omitempty"`
	Password     string `json:"password,omitempty" yaml:"password,omitempty"`
	Encrypt      string `json:"encrypt" yaml:"encrypt"`
	Compress     int    `json:"compress" yaml:"compress"`
	Priority     int    `json:"priority" yaml:"priority"`         // 优先级: 1 级 > 2 级 (数值越小优先级越高，默认 10)
	Bandwidth    int    `json:"bandwidth" yaml:"bandwidth"`       // 标称带宽权重 (Mbps)，同优先级下越大越优先，默认 100
	KcpFecData   int    `json:"kcpfecdata" yaml:"kcpfecdata"`     // KCP FEC 数据分片 (如 10)
	KcpFecParity int    `json:"kcpfecparity" yaml:"kcpfecparity"` // KCP FEC 校验分片 (如 3)
	RdnsDomain   string `json:"rdnsdomain,omitempty" yaml:"rdnsdomain,omitempty"`
	RdnsUpstream string `json:"rdnsupstream,omitempty" yaml:"rdnsupstream,omitempty"`
	RdnsPsk      string `json:"rdnspsk,omitempty" yaml:"rdnspsk,omitempty"`
	RdnsQueryType string `json:"rdnsquerytype,omitempty" yaml:"rdnsquerytype,omitempty"`
	// Alive is a configuration/status hint carried in JSON. Runtime liveness
	// is tracked atomically inside Manager (see Manager.NodeAlive); this field
	// is never read by failover logic.
	Alive bool `json:"alive"`
}

// IsSocks5 reports whether this node dials a plain SOCKS5 upstream instead of
// an SPP channel.
func (n *Node) IsSocks5() bool {
	return strings.EqualFold(strings.TrimSpace(n.Type), "socks5")
}

// DialAddr is the address an upstream connection goes to. socks5 nodes split
// host and port across server+port; SPP channels carry both in server.
func (n *Node) DialAddr() string {
	if n.IsSocks5() && n.Port > 0 {
		return net.JoinHostPort(strings.TrimSpace(n.Server), strconv.Itoa(n.Port))
	}
	return n.Server
}

// getEffectivePriority returns the configured priority, defaulting to 10 if <= 0.
func (n *Node) getEffectivePriority() int {
	if n.Priority <= 0 {
		return 10
	}
	return n.Priority
}

// getEffectiveBandwidth returns the configured bandwidth, defaulting to 100 if <= 0.
func (n *Node) getEffectiveBandwidth() int {
	if n.Bandwidth <= 0 {
		return 100
	}
	return n.Bandwidth
}

const (
	defaultHealthCheckInterval = 10 * time.Second
	nodeProbeTimeout           = 3 * time.Second
)

// dialNetworkForProto returns the IP protocol used to reach an SPP server.
// Only plain "tcp" and "rhttp" use TCP; udp/rudp/kcp/quic/rdns all ride on UDP.
func dialNetworkForProto(proto string) string {
	switch strings.ToLower(strings.TrimSpace(proto)) {
	case "", "tcp", "rhttp":
		return "tcp"
	default:
		return "udp"
	}
}

// probeTCP checks reachability with a TCP connect.
func probeTCP(addr string, timeout time.Duration) (bool, time.Duration) {
	start := time.Now()
	conn, err := netdial.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false, 0
	}
	_ = conn.Close()
	return true, time.Since(start)
}

// probeUDPAddr performs a cheap UDP dial/write. It must NEVER call gohome's
// RudpConn/Kcp/QUIC Dial: those allocate multi-MB FrameMgr buffers, and the
// previous timed-out probe path leaked them (pprof: ~95% inuse via
// probeNodeWithRTT -> RudpConn.Dial -> NewFrameMgr -> NewRBuffergo).
//
// A successful UDP write does not prove the SPP handshake works; proxy
// failures (ReportNodeFailure) remain the primary down signal for UDP-family
// nodes. This probe mainly catches DNS/resolve failures and invalid addresses.
func probeUDPAddr(addr string, timeout time.Duration) (bool, time.Duration) {
	if timeout <= 0 {
		timeout = nodeProbeTimeout
	}
	start := time.Now()
	d := netdial.Dialer(timeout)
	conn, err := d.Dial("udp", addr)
	if err != nil {
		return false, 0
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write([]byte{0}); err != nil {
		return false, 0
	}
	return true, time.Since(start)
}

// probeNodeWithRTT checks whether a node looks reachable.
// TCP/rhttp: real TCP connect. PingTunnel: dedicated ICMP probe.
// udp/rudp/kcp/quic/rdns: lightweight UDP only (see probeUDPAddr).
func probeNodeWithRTT(node *Node, timeout time.Duration) (bool, time.Duration) {
	// A socks5 node is a plain TCP endpoint: reachability is a connect to
	// its dial address, and the SOCKS5 handshake itself is exercised on
	// first use by the proxy caller.
	if node.IsSocks5() {
		return probeTCP(node.DialAddr(), timeout)
	}
	if isPingTunnelProto(node.ServerProto) {
		return probePingTunnel(node, timeout)
	}
	proto := strings.ToLower(strings.TrimSpace(node.ServerProto))
	switch proto {
	case "", "tcp", "rhttp":
		return probeTCP(node.Server, timeout)
	case "rdns":
		addr := node.Server
		if strings.TrimSpace(node.RdnsUpstream) != "" {
			addr = node.RdnsUpstream
		}
		return probeUDPAddr(addr, timeout)
	default:
		// udp, rudp, kcp, quic, ricmp, ...
		return probeUDPAddr(node.Server, timeout)
	}
}

// probeNode maintains backward compatibility for unit tests.
func probeNode(node *Node) bool {
	alive, _ := probeNodeWithRTT(node, nodeProbeTimeout)
	return alive
}

// protoRobustnessBase returns the baseline robustness weight for a transport protocol.
// Weights are aligned with empirical downlink (Server -> Client) benchmark throughput:
// RUDP  (3.33 MB/s Peak / 2.25 MB/s Avg): 100 - Highest downlink throughput with batch UDP I/O & 4MB buffer
// RICMP (1.03 MB/s Peak / 0.29 MB/s Avg): 95  - High downlink throughput via ICMP ping tunnel
// KCP   (0.35 MB/s Peak / 0.11 MB/s Avg): 85  - Reliable UDP protocol with FEC
// PingTunnel / ICMP                     : 70  - Standalone ICMP ping tunnel
// TCP   (0.17 MB/s Peak / 0.09 MB/s Avg): 50  - Standard TCP baseline over lossy WAN
// RHTTP (0.10 MB/s Peak / 0.05 MB/s Avg): 25  - HTTP-encapsulated tunnel; client polling overhead
// RDNS  (0.01 MB/s Peak / 0.00 MB/s Avg): 15  - DNS tunnel; bypasses captive portals and DNS-only egress
// QUIC  (0.01 MB/s Peak / 0.00 MB/s Avg): 10  - User-space UDP stream downlink limited on lossy WAN
func protoRobustnessBase(proto string) int {
	switch strings.ToLower(strings.TrimSpace(proto)) {
	case "rudp":
		return 100
	case "ricmp":
		return 95
	case "kcp":
		return 85
	case ProtoPingTunnel, "icmp":
		return 70
	case "tcp", "":
		return 50
	case "rhttp":
		return 25
	case "rdns":
		return 15
	case "quic":
		return 10
	default:
		return 30
	}
}

// upstream is one running client (SPP or pingtunnel) exposing a local SOCKS5.
type upstream interface {
	Close() error
	HasRecentTraffic(window time.Duration) bool
}

type nodeRuntime struct {
	node             *Node
	localSocks5      string
	client           upstream
	clientPtr        atomic.Pointer[upstream]
	alive            atomic.Bool
	rttMs            atomic.Int64
	consecutiveOks   atomic.Int32
	consecutiveFails atomic.Int32
	activeConns      atomic.Int64
	lastUploadNano   atomic.Int64
	lastDownloadNano atomic.Int64
}

func (nr *nodeRuntime) hasRecentTraffic(window time.Duration) bool {
	if window <= 0 {
		window = defaultHealthCheckInterval
	}
	now := time.Now().UnixNano()
	winNs := window.Nanoseconds()
	up := nr.lastUploadNano.Load()
	down := nr.lastDownloadNano.Load()
	if up > 0 && down > 0 && (now-up) <= winNs && (now-down) <= winNs {
		return true
	}
	if cl := nr.clientPtr.Load(); cl != nil && (*cl).HasRecentTraffic(window) {
		return true
	}
	return false
}

// calcCost computes connection selection cost:
// Lower is better. Incorporates RTT, active connections, probe failures, bandwidth, and protocol robustness.
func (nr *nodeRuntime) calcCost() float64 {
	rtt := float64(nr.rttMs.Load())
	if rtt < 10.0 {
		rtt = 10.0
	}
	conns := float64(nr.activeConns.Load())
	fails := float64(nr.consecutiveFails.Load())
	bw := float64(nr.node.getEffectiveBandwidth())
	if bw <= 0 {
		bw = 100.0
	}
	robust := float64(protoRobustnessBase(nr.node.ServerProto)) / 100.0
	if robust <= 0.1 {
		robust = 0.7
	}

	cost := (rtt * (conns + 1.0) * (1.0 + fails*2.0)) / (bw * robust)
	return cost
}

// robustnessScore calculates the runtime robustness score for candidate selection.
func (nr *nodeRuntime) robustnessScore() int {
	base := protoRobustnessBase(nr.node.ServerProto)
	oks := int(nr.consecutiveOks.Load())
	fails := int(nr.consecutiveFails.Load())
	rtt := nr.rttMs.Load()

	score := base + (oks * 2) - (fails * 20)
	if rtt > 100 {
		score -= int((rtt - 100) / 50)
	}
	return score
}

// isBetterThan compares two nodes in the same priority tier:
// Rules: "选更稳健其次带宽大的" (Robustness first, then Bandwidth).
func (a *nodeRuntime) isBetterThan(b *nodeRuntime) bool {
	scoreA := a.robustnessScore()
	scoreB := b.robustnessScore()

	// If robustness difference is noticeable (> 10 points), more robust wins
	if scoreA-scoreB > 10 {
		return true
	}
	if scoreB-scoreA > 10 {
		return false
	}

	// In the same robustness tier, higher configured bandwidth wins
	bwA := a.node.getEffectiveBandwidth()
	bwB := b.node.getEffectiveBandwidth()
	if bwA != bwB {
		return bwA > bwB
	}

	// Bandwidth equal: higher score, then lower RTT
	if scoreA != scoreB {
		return scoreA > scoreB
	}
	rttA := a.rttMs.Load()
	rttB := b.rttMs.Load()
	if rttA > 0 && rttB > 0 {
		return rttA < rttB
	}
	return true
}

// Manager manages multiple SPP channels with priority, health probing and automatic failover/preemption.
type Manager struct {
	nodes          []*Node
	runtimes       []*nodeRuntime
	activeClient   upstream
	activeIndex    int32 // -1 while no node is active
	localSocks5    string
	checkStopCh    chan struct{}
	checkStopOnce  sync.Once
	healthDoneCh   chan struct{}
	triggerCheckCh chan struct{}
	closed         atomic.Bool
	healthInterval time.Duration
	mu             sync.Mutex
}

// NewManager creates a manager for the given nodes.
func NewManager(nodes []*Node, localSocks5 string) (*Manager, error) {
	return newManager(nodes, localSocks5, defaultHealthCheckInterval)
}

func newManager(nodes []*Node, localSocks5 string, interval time.Duration) (*Manager, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no spp nodes provided")
	}
	if localSocks5 == "" {
		localSocks5 = "127.0.0.1:10808"
	}
	if interval <= 0 {
		interval = defaultHealthCheckInterval
	}

	host, portStr, err := net.SplitHostPort(localSocks5)
	basePort := 10808
	if err == nil {
		if p, err := strconv.Atoi(portStr); err == nil {
			basePort = p
		}
	} else {
		host = "127.0.0.1"
	}

	runtimes := make([]*nodeRuntime, len(nodes))
	for i, n := range nodes {
		runtimes[i] = &nodeRuntime{
			node:        n,
			localSocks5: fmt.Sprintf("%s:%d", host, basePort+i),
		}
	}

	m := &Manager{
		nodes:          nodes,
		runtimes:       runtimes,
		localSocks5:    localSocks5,
		checkStopCh:    make(chan struct{}),
		healthDoneCh:   make(chan struct{}),
		triggerCheckCh: make(chan struct{}, 1),
		activeIndex:    -1,
		healthInterval: interval,
	}

	// Initial synchronous probe
	m.probeAllNodes()

	// Only start clients for nodes that probed alive. Do not keep a reconnecting
	// client when every upstream is down.
	aliveCands := m.getAliveCandidatesLocked()
	if len(aliveCands) == 0 {
		loggo.Error("[SPP Manager] All %d SPP nodes are unreachable at startup; waiting for health checks", len(nodes))
		atomic.StoreInt32(&m.activeIndex, -1)
	} else {
		for _, rt := range aliveCands {
			if err := m.ensureClientRunningLocked(rt); err != nil {
				rt.alive.Store(false)
			}
		}
		aliveCands = m.getAliveCandidatesLocked()
		best := m.pickCandidateLocked(aliveCands)
		if best == nil {
			atomic.StoreInt32(&m.activeIndex, -1)
		} else {
			for i, rt := range m.runtimes {
				if rt == best {
					atomic.StoreInt32(&m.activeIndex, int32(i))
					break
				}
			}
		}
	}

	go m.healthLoop()
	return m, nil
}

// udpFamilyProbeNeedsConfirm is how many consecutive cheap UDP/pingtunnel probe
// successes are required before reviving a node that was marked down by proxy
// failures. Cheap probes cannot see SPP handshake failure, so a single success
// must not undo ReportNodeFailure immediately.
const udpFamilyProbeNeedsConfirm = int32(2)

// probeAllNodes probes all nodes in parallel and records results in runtimes.
// If a node is already alive and has recent bidirectional traffic ("有数据在有来有回"),
// skip synthetic dial probing to avoid false timeouts when a narrow pipe is saturated.
func (m *Manager) probeAllNodes() {
	var wg sync.WaitGroup
	for i := range m.runtimes {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rt := m.runtimes[idx]
			if rt.alive.Load() && rt.hasRecentTraffic(m.healthInterval) {
				rt.consecutiveOks.Add(1)
				rt.consecutiveFails.Store(0)
				return
			}
			alive, rtt := probeNodeWithRTT(rt.node, nodeProbeTimeout)
			if !alive && rt.alive.Load() && rt.hasRecentTraffic(m.healthInterval) {
				rt.consecutiveOks.Add(1)
				rt.consecutiveFails.Store(0)
				return
			}
			if !alive {
				rt.alive.Store(false)
				rt.consecutiveFails.Add(1)
				rt.consecutiveOks.Store(0)
				return
			}

			rt.rttMs.Store(rtt.Milliseconds())
			proto := strings.ToLower(strings.TrimSpace(rt.node.ServerProto))
			cheapProbe := dialNetworkForProto(proto) == "udp" || isPingTunnelProto(proto)
			if cheapProbe && rt.consecutiveFails.Load() >= 2 {
				// Node was marked down by proxy failures; require sustained cheap
				// probe success before treating it as alive again.
				oks := rt.consecutiveOks.Add(1)
				if oks < udpFamilyProbeNeedsConfirm {
					rt.alive.Store(false)
					return
				}
				rt.alive.Store(true)
				rt.consecutiveFails.Store(0)
				return
			}
			rt.alive.Store(true)
			rt.consecutiveOks.Add(1)
			rt.consecutiveFails.Store(0)
		}(i)
	}
	wg.Wait()
}

// getActivePriorityTierLocked finds the highest effective priority (lowest number)
// among currently alive nodes. If no nodes are alive, falls back to the highest
// priority among all configured nodes.
func (m *Manager) getActivePriorityTierLocked() int {
	minPri := -1
	for _, rt := range m.runtimes {
		if rt.alive.Load() {
			pri := rt.node.getEffectivePriority()
			if minPri == -1 || pri < minPri {
				minPri = pri
			}
		}
	}
	if minPri != -1 {
		return minPri
	}
	for _, rt := range m.runtimes {
		pri := rt.node.getEffectivePriority()
		if minPri == -1 || pri < minPri {
			minPri = pri
		}
	}
	return minPri
}

// getActiveTierCandidatesLocked returns all runtimes belonging to the current active tier.
// Alive candidates are prioritized; if none in the active tier are alive, alive candidates
// in any tier are returned; if all nodes are down, all nodes in the active tier are returned.
func (m *Manager) getActiveTierCandidatesLocked() []*nodeRuntime {
	tier := m.getActivePriorityTierLocked()
	if tier == -1 {
		return nil
	}

	var aliveInTier []*nodeRuntime
	var allInTier []*nodeRuntime

	for _, rt := range m.runtimes {
		if rt.node.getEffectivePriority() == tier {
			allInTier = append(allInTier, rt)
			if rt.alive.Load() {
				aliveInTier = append(aliveInTier, rt)
			}
		}
	}

	if len(aliveInTier) > 0 {
		return aliveInTier
	}

	var anyAlive []*nodeRuntime
	for _, rt := range m.runtimes {
		if rt.alive.Load() {
			anyAlive = append(anyAlive, rt)
		}
	}
	if len(anyAlive) > 0 {
		return anyAlive
	}

	return allInTier
}

// pickCandidateLocked picks the best runtime candidate among the candidates using dynamic cost:
// cost = (RTT * (ActiveConns + 1) * (1 + Fails * 2)) / (Bandwidth * Robustness)
// Lower cost wins. When costs are tied, earlier index in candidate list is preserved.
func (m *Manager) pickCandidateLocked(candidates []*nodeRuntime) *nodeRuntime {
	if len(candidates) == 0 {
		return nil
	}
	best := candidates[0]
	bestCost := best.calcCost()

	for _, c := range candidates[1:] {
		cost := c.calcCost()
		if cost < bestCost-1e-6 {
			best = c
			bestCost = cost
		}
	}
	return best
}

// stopClientLocked closes a node's local SPP/pingtunnel client so reconnect
// buffers can be reclaimed. Caller holds m.mu.
func (m *Manager) stopClientLocked(rt *nodeRuntime) {
	if rt == nil || rt.client == nil {
		return
	}
	rt.clientPtr.Store(nil)
	_ = rt.client.Close()
	rt.client = nil
	loggo.Info("[SPP Manager] Channel [%s] client stopped", rt.node.Name)
}

// getAliveCandidatesLocked returns only currently-alive candidates for
// starting or picking a local upstream client.
func (m *Manager) getAliveCandidatesLocked() []*nodeRuntime {
	cands := m.getActiveTierCandidatesLocked()
	out := make([]*nodeRuntime, 0, len(cands))
	for _, rt := range cands {
		if rt.alive.Load() {
			out = append(out, rt)
		}
	}
	return out
}

// ensureClientRunningLocked starts the SPP client for a node if not already
// running. socks5 nodes need no client: the node's dial address and
// credentials are used directly by the proxy caller.
func (m *Manager) ensureClientRunningLocked(rt *nodeRuntime) error {
	if m.closed.Load() {
		return fmt.Errorf("spp manager is closed")
	}
	if rt.node.IsSocks5() {
		return nil
	}
	if rt.client != nil {
		return nil
	}
	target := rt.node
	cfg := &Config{
		ServerProto:   target.ServerProto,
		Server:        target.Server,
		Key:           target.Key,
		Encrypt:       target.Encrypt,
		Compress:      target.Compress,
		LocalSocks5:   rt.localSocks5,
		Name:          target.Name,
		KcpFecData:    target.KcpFecData,
		KcpFecParity:  target.KcpFecParity,
		RdnsDomain:    target.RdnsDomain,
		RdnsUpstream:  target.RdnsUpstream,
		RdnsPsk:       target.RdnsPsk,
		RdnsQueryType: target.RdnsQueryType,
	}
	var cl upstream
	var err error
	if isPingTunnelProto(target.ServerProto) {
		cl, err = newPingTunnelClient(cfg)
	} else {
		cl, err = NewClient(cfg)
	}
	if err != nil {
		loggo.Warn("[SPP Manager] Failed to start client for channel [%s] (%s): %v", target.Name, rt.localSocks5, err)
		return err
	}
	rt.client = cl
	rt.clientPtr.Store(&cl)
	loggo.Info("[SPP Manager] Channel [%s] client started on %s", target.Name, rt.localSocks5)
	return nil
}

// PickNode dynamically and intelligently selects an upstream node from the
// active priority tier. Returns (dialAddr, user, pass, nodeName, release).
// SPP nodes dial their local client listener with no credentials; socks5
// nodes dial the real server with the credentials configured on the node,
// so each node in the pool carries its own auth. When no node is alive the
// address comes back empty so the proxy fails fast instead of dialing a
// dead upstream.
func (m *Manager) PickNode() (string, string, string, string, func()) {
	if m.closed.Load() {
		return "", "", "", "", nil
	}

	m.mu.Lock()
	candidates := m.getAliveCandidatesLocked()
	if len(candidates) == 0 {
		m.mu.Unlock()
		return "", "", "", "", nil
	}

	chosen := m.pickCandidateLocked(candidates)
	if chosen == nil {
		chosen = candidates[0]
	}

	if err := m.ensureClientRunningLocked(chosen); err != nil {
		m.mu.Unlock()
		return "", "", "", "", nil
	}
	m.mu.Unlock()

	chosen.activeConns.Add(1)
	var once sync.Once
	release := func() {
		once.Do(func() {
			chosen.activeConns.Add(-1)
		})
	}
	n := chosen.node
	if n.IsSocks5() {
		return n.DialAddr(), n.Username, n.Password, n.Name, release
	}
	return chosen.localSocks5, "", "", n.Name, release
}

// switchNode starts the client at index and sets it as the activeIndex for backward compatibility.
func (m *Manager) switchNode(index int) error {
	if m.closed.Load() {
		return fmt.Errorf("spp manager is closed")
	}
	if index < 0 || index >= len(m.nodes) {
		return fmt.Errorf("node index out of range")
	}

	rt := m.runtimes[index]
	if err := m.ensureClientRunningLocked(rt); err != nil {
		rt.alive.Store(false)
		return err
	}
	rt.alive.Store(true)
	atomic.StoreInt32(&m.activeIndex, int32(index))
	return nil
}

// fallbackLocked selects the next best reachable node excluding exclude.
func (m *Manager) fallbackLocked(exclude int) int {
	bestIdx := -1
	for i := range m.nodes {
		if i == exclude {
			continue
		}
		if !m.runtimes[i].alive.Load() {
			continue
		}
		if bestIdx < 0 {
			bestIdx = i
			continue
		}
		priCurr := m.nodes[i].getEffectivePriority()
		priBest := m.nodes[bestIdx].getEffectivePriority()
		if priCurr < priBest || (priCurr == priBest && m.runtimes[i].isBetterThan(m.runtimes[bestIdx])) {
			bestIdx = i
		}
	}

	if bestIdx >= 0 {
		if err := m.switchNode(bestIdx); err == nil {
			loggo.Info("[SPP Manager] Fallback succeeded to channel [%s]", m.nodes[bestIdx].Name)
			return bestIdx
		}
	}

	loggo.Error("[SPP Manager] All SPP channels are unreachable!")
	return -1
}

func (m *Manager) healthLoop() {
	defer close(m.healthDoneCh)
	ticker := time.NewTicker(m.healthInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.checkStopCh:
			return
		case <-ticker.C:
			m.checkNodes()
		case <-m.triggerCheckCh:
			m.checkNodes()
		}
	}
}

// checkNodes executes a full health sweep, stops clients for dead nodes, and
// starts clients only for alive nodes in the active tier.
func (m *Manager) checkNodes() {
	m.probeAllNodes()

	if m.closed.Load() {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed.Load() {
		return
	}

	for _, rt := range m.runtimes {
		if !rt.alive.Load() {
			m.stopClientLocked(rt)
		}
	}

	candidates := m.getAliveCandidatesLocked()
	for _, rt := range candidates {
		if err := m.ensureClientRunningLocked(rt); err != nil {
			rt.alive.Store(false)
			m.stopClientLocked(rt)
		}
	}
	candidates = m.getAliveCandidatesLocked()

	best := m.pickCandidateLocked(candidates)
	if best != nil {
		for i, rt := range m.runtimes {
			if rt == best {
				atomic.StoreInt32(&m.activeIndex, int32(i))
				break
			}
		}
	} else {
		atomic.StoreInt32(&m.activeIndex, -1)
	}
}

// RecordNodeTraffic records active proxy payload bytes on the given node.
// When both upload and download bytes are flowing, the node is known to be alive
// without needing synthetic probe connections.
func (m *Manager) RecordNodeTraffic(nodeName string, uploadBytes, downloadBytes int64) {
	if m == nil || m.closed.Load() || nodeName == "" {
		return
	}
	now := time.Now().UnixNano()
	for _, rt := range m.runtimes {
		if rt.node.Name == nodeName {
			if uploadBytes > 0 {
				rt.lastUploadNano.Store(now)
			}
			if downloadBytes > 0 {
				rt.lastDownloadNano.Store(now)
			}
			if uploadBytes > 0 && downloadBytes > 0 {
				rt.alive.Store(true)
				rt.consecutiveFails.Store(0)
			}
			return
		}
	}
}

// HasRecentNodeTraffic reports whether the given node has active bidirectional
// wire traffic within window (including SPP tunnel traffic draining in flight).
func (m *Manager) HasRecentNodeTraffic(nodeName string, window time.Duration) bool {
	if m == nil || m.closed.Load() || nodeName == "" {
		return false
	}
	for _, rt := range m.runtimes {
		if rt.node.Name == nodeName {
			return rt.hasRecentTraffic(window)
		}
	}
	return false
}

// ReportNodeFailure is called when proxying through a specific channel fails.
// After a few consecutive failures the node is marked down immediately (UDP-family
// light probes cannot see SPP handshake failure) and a single shared health sweep
// is scheduled via triggerCheckCh — never one probe goroutine per failure.
func (m *Manager) ReportNodeFailure(nodeName string, err error) {
	if m.closed.Load() || nodeName == "" {
		return
	}
	for _, rt := range m.runtimes {
		if rt.node.Name != nodeName {
			continue
		}
		if rt.alive.Load() && rt.hasRecentTraffic(m.healthInterval) {
			return
		}
		fails := rt.consecutiveFails.Add(1)
		rt.consecutiveOks.Store(0)
		if fails >= 2 {
			wasAlive := rt.alive.Swap(false)
			if wasAlive {
				loggo.Warn("[SPP Manager] Channel [%s] marked down after %d failures (%v), scheduling health check",
					nodeName, fails, err)
			}
			select {
			case m.triggerCheckCh <- struct{}{}:
			default:
			}
		}
		return
	}
}

// ReportFailure maintains backward compatibility with older callers.
func (m *Manager) ReportFailure(err error) {
	name := m.ActiveNodeName()
	if name != "" {
		m.ReportNodeFailure(name, err)
	}
}

// ActiveNodeIndex returns the index of the currently active node, -1 if none.
func (m *Manager) ActiveNodeIndex() int {
	return int(atomic.LoadInt32(&m.activeIndex))
}

// ActiveNodeName returns the name of the currently active node.
func (m *Manager) ActiveNodeName() string {
	idx := m.ActiveNodeIndex()
	if idx < 0 || idx >= len(m.nodes) {
		return ""
	}
	return m.nodes[idx].Name
}

// NodeStatus is a node's identity, priority, bandwidth, RTT and liveness.
type NodeStatus struct {
	Name        string `json:"name"`
	Server      string `json:"server"`
	Proto       string `json:"proto"`
	Priority    int    `json:"priority"`
	Bandwidth   int    `json:"bandwidth"`
	RTTMs       int64  `json:"rtt_ms"`
	Alive       bool   `json:"alive"`
	Active      bool   `json:"active"`
	ActiveConns int64  `json:"active_conns"`
}

// NodeStatuses returns status for every configured node.
// All alive nodes in the currently active priority tier are marked Active: true.
func (m *Manager) NodeStatuses() []NodeStatus {
	m.mu.Lock()
	tier := m.getActivePriorityTierLocked()
	m.mu.Unlock()

	out := make([]NodeStatus, 0, len(m.nodes))
	for i, n := range m.nodes {
		rtt := int64(0)
		alive := false
		var conns int64
		if i < len(m.runtimes) {
			rt := m.runtimes[i]
			rtt = rt.rttMs.Load()
			alive = rt.alive.Load()
			conns = rt.activeConns.Load()
		}
		isActive := alive && (n.getEffectivePriority() == tier)
		out = append(out, NodeStatus{
			Name:        n.Name,
			Server:      n.Server,
			Proto:       n.ServerProto,
			Priority:    n.getEffectivePriority(),
			Bandwidth:   n.getEffectiveBandwidth(),
			RTTMs:       rtt,
			Alive:       alive,
			Active:      isActive,
			ActiveConns: conns,
		})
	}
	return out
}

// NodeAlive reports the latest probe result for the given node index.
func (m *Manager) NodeAlive(index int) bool {
	if index < 0 || index >= len(m.runtimes) {
		return false
	}
	return m.runtimes[index].alive.Load()
}

// Socks5Addr is the local SOCKS5 address exposed by the SPP client.
func (m *Manager) Socks5Addr() string {
	addr, _, _, _, release := m.PickNode()
	if release != nil {
		release()
	}
	if addr != "" {
		return addr
	}
	return m.localSocks5
}

// Socks5Auth returns empty credentials for local SOCKS5.
func (m *Manager) Socks5Auth() (string, string) {
	return "", ""
}

func (m *Manager) stopHealthLoop() {
	m.checkStopOnce.Do(func() {
		close(m.checkStopCh)
	})
	<-m.healthDoneCh
}

// Close stops health checks and all active SPP clients.
func (m *Manager) Close() {
	if !m.closed.CompareAndSwap(false, true) {
		return
	}
	m.stopHealthLoop()

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rt := range m.runtimes {
		m.stopClientLocked(rt)
	}
	if m.activeClient != nil {
		_ = m.activeClient.Close()
		m.activeClient = nil
	}
}
