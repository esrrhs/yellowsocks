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
	"github.com/esrrhs/gohome/network"
)

// Node is one upstream SPP channel/server configuration.
type Node struct {
	Name         string `json:"name" yaml:"name"`
	Server       string `json:"server" yaml:"server"`
	ServerProto  string `json:"server_proto" yaml:"server_proto"` // tcp, udp, rudp, kcp, quic
	Key          string `json:"key" yaml:"key"`
	Encrypt      string `json:"encrypt" yaml:"encrypt"`
	Compress     int    `json:"compress" yaml:"compress"`
	Priority     int    `json:"priority" yaml:"priority"`         // 优先级: 1 级 > 2 级 (数值越小优先级越高，默认 10)
	Bandwidth    int    `json:"bandwidth" yaml:"bandwidth"`       // 标称带宽权重 (Mbps)，同优先级下越大越优先，默认 100
	KcpFecData   int    `json:"kcpfecdata" yaml:"kcpfecdata"`     // KCP FEC 数据分片 (如 10)
	KcpFecParity int    `json:"kcpfecparity" yaml:"kcpfecparity"` // KCP FEC 校验分片 (如 3)
	// Alive is a configuration/status hint carried in JSON. Runtime liveness
	// is tracked atomically inside Manager (see Manager.NodeAlive); this field
	// is never read by failover logic.
	Alive bool `json:"alive"`
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
// Only plain "tcp" is TCP; udp/rudp/kcp/quic all ride on UDP.
func dialNetworkForProto(proto string) string {
	switch strings.ToLower(strings.TrimSpace(proto)) {
	case "", "tcp":
		return "tcp"
	default:
		return "udp"
	}
}

// probeNodeWithRTT performs a protocol-level handshake probe against the node.
// For TCP: performs TCP 3-way handshake.
// For UDP/RUDP/KCP/QUIC: dials using network.NewConn and performs protocol handshake.
// Returns (alive, rtt).
func probeNodeWithRTT(node *Node, timeout time.Duration) (bool, time.Duration) {
	proto := strings.ToLower(strings.TrimSpace(node.ServerProto))
	if proto == "" || proto == "tcp" {
		start := time.Now()
		conn, err := net.DialTimeout("tcp", node.Server, timeout)
		if err != nil {
			return false, 0
		}
		_ = conn.Close()
		return true, time.Since(start)
	}

	dialer, err := network.NewConn(proto)
	if err != nil || dialer == nil {
		return false, 0
	}
	defer dialer.Close()

	type dialResult struct {
		conn network.Conn
		err  error
		rtt  time.Duration
	}
	ch := make(chan dialResult, 1)
	start := time.Now()

	go func() {
		c, err := dialer.Dial(node.Server)
		ch <- dialResult{conn: c, err: err, rtt: time.Since(start)}
	}()

	select {
	case res := <-ch:
		if res.err != nil || res.conn == nil {
			return false, 0
		}
		_ = res.conn.Close()
		return true, res.rtt
	case <-time.After(timeout):
		return false, 0
	}
}

// probeNode maintains backward compatibility for unit tests.
func probeNode(node *Node) bool {
	alive, _ := probeNodeWithRTT(node, nodeProbeTimeout)
	return alive
}

// protoRobustnessBase returns the baseline robustness weight for a transport protocol.
// Under lossy cross-border WAN conditions:
// TCP: kernel-level congestion control & SACK (most robust against random drop)
// KCP: FEC redundancy option (highly robust when configured)
// QUIC: user-space multi-stream over UDP
// RUDP: basic stop-and-wait / window UDP
func protoRobustnessBase(proto string) int {
	switch strings.ToLower(strings.TrimSpace(proto)) {
	case "tcp", "":
		return 100
	case "kcp":
		return 95
	case "quic":
		return 85
	case "rudp":
		return 75
	default:
		return 70
	}
}

type nodeRuntime struct {
	node             *Node
	localSocks5      string
	client           *Client
	alive            atomic.Bool
	rttMs            atomic.Int64
	consecutiveOks   atomic.Int32
	consecutiveFails atomic.Int32
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
	activeClient   *Client
	activeIndex    int32 // -1 while no node is active
	localSocks5    string
	checkStopCh    chan struct{}
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
		triggerCheckCh: make(chan struct{}, 1),
		activeIndex:    -1,
		healthInterval: interval,
	}

	// Initial synchronous probe
	m.probeAllNodes()

	// Select best node according to priority, robustness and bandwidth
	bestIdx := m.selectBestNodeLocked()
	if bestIdx < 0 {
		loggo.Error("[SPP Manager] All %d SPP nodes are unreachable at startup; starting node 0 anyway", len(nodes))
		bestIdx = 0
	}
	if err := m.switchNode(bestIdx); err != nil {
		loggo.Warn("[SPP Manager] Initial active node %d [%s] failed to start: %v, trying fallback", bestIdx, m.nodes[bestIdx].Name, err)
		if fb := m.fallbackLocked(bestIdx); fb < 0 {
			loggo.Error("[SPP Manager] No SPP node could be started")
		}
	}

	go m.healthLoop()
	return m, nil
}

// probeAllNodes probes all nodes in parallel and records results in runtimes.
func (m *Manager) probeAllNodes() {
	var wg sync.WaitGroup
	for i := range m.runtimes {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			rt := m.runtimes[idx]
			alive, rtt := probeNodeWithRTT(rt.node, nodeProbeTimeout)
			rt.alive.Store(alive)
			if alive {
				rt.rttMs.Store(rtt.Milliseconds())
				rt.consecutiveOks.Add(1)
				rt.consecutiveFails.Store(0)
			} else {
				rt.consecutiveFails.Add(1)
				rt.consecutiveOks.Store(0)
			}
		}(i)
	}
	wg.Wait()
}

// selectBestNodeLocked selects the optimal node:
// 1. Gather all currently alive nodes.
// 2. Find the highest priority level (minimum Priority number, e.g. 1 > 2).
// 3. Among alive nodes in that highest priority level, select the best by robustness and bandwidth.
// If all nodes are down, returns the configured node with highest priority.
func (m *Manager) selectBestNodeLocked() int {
	var aliveIndices []int
	for i := range m.runtimes {
		if m.runtimes[i].alive.Load() {
			aliveIndices = append(aliveIndices, i)
		}
	}

	if len(aliveIndices) == 0 {
		return -1
	}

	// Find best priority among alive nodes
	bestPri := m.nodes[aliveIndices[0]].getEffectivePriority()
	for _, idx := range aliveIndices[1:] {
		if pri := m.nodes[idx].getEffectivePriority(); pri < bestPri {
			bestPri = pri
		}
	}

	// Filter alive nodes that match the best priority
	var topCandidates []int
	for _, idx := range aliveIndices {
		if m.nodes[idx].getEffectivePriority() == bestPri {
			topCandidates = append(topCandidates, idx)
		}
	}

	// Among top priority candidates, pick the one that is most robust, then highest bandwidth
	bestIdx := topCandidates[0]
	for _, idx := range topCandidates[1:] {
		if m.runtimes[idx].isBetterThan(m.runtimes[bestIdx]) {
			bestIdx = idx
		}
	}
	return bestIdx
}

// switchNode stops the active client and starts the one at index.
// Caller must hold m.mu (or be the constructor before healthLoop starts).
func (m *Manager) switchNode(index int) error {
	if m.closed.Load() {
		return fmt.Errorf("spp manager is closed")
	}
	if index < 0 || index >= len(m.nodes) {
		return fmt.Errorf("node index out of range")
	}

	rt := m.runtimes[index]
	target := rt.node
	loggo.Info("[SPP Manager] Switching active channel to [%s] (%s via %s, priority=%d, bw=%d, local=%s)...",
		target.Name, target.Server, target.ServerProto, target.getEffectivePriority(), target.getEffectiveBandwidth(), rt.localSocks5)

	// Keep existing client connections intact so in-flight transfers (e.g. git clone, long streams) drain gracefully without being killed!
	if rt.client == nil {
		cfg := &Config{
			ServerProto:  target.ServerProto,
			Server:       target.Server,
			Key:          target.Key,
			Encrypt:      target.Encrypt,
			Compress:     target.Compress,
			LocalSocks5:  rt.localSocks5,
			Name:         target.Name,
			KcpFecData:   target.KcpFecData,
			KcpFecParity: target.KcpFecParity,
		}

		cl, err := NewClient(cfg)
		if err != nil {
			rt.alive.Store(false)
			return err
		}
		rt.client = cl
	}

	rt.alive.Store(true)
	atomic.StoreInt32(&m.activeIndex, int32(index))
	loggo.Info("[SPP Manager] Active channel set to [%s] (local socks5 %s)", target.Name, rt.localSocks5)
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

// checkNodes executes a full health sweep and handles failover / preemption.
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

	currIdx := int(atomic.LoadInt32(&m.activeIndex))
	bestIdx := m.selectBestNodeLocked()

	if currIdx < 0 {
		if bestIdx >= 0 {
			_ = m.switchNode(bestIdx)
		}
		return
	}

	currAlive := currIdx < len(m.runtimes) && m.runtimes[currIdx].alive.Load()
	if !currAlive {
		if bestIdx < 0 || !m.runtimes[bestIdx].alive.Load() {
			// All nodes down; do not flap
			return
		}
		loggo.Warn("[SPP Manager] Current active channel [%s] is down, failing over to [%s] (priority %d)...",
			m.nodes[currIdx].Name, m.nodes[bestIdx].Name, m.nodes[bestIdx].getEffectivePriority())
		_ = m.switchNode(bestIdx)
		return
	}

	// Preemption / Failback:
	// Only preempt if bestIdx is valid, actually alive, and has higher priority
	if bestIdx >= 0 && m.runtimes[bestIdx].alive.Load() {
		currPri := m.nodes[currIdx].getEffectivePriority()
		bestPri := m.nodes[bestIdx].getEffectivePriority()
		if bestPri < currPri {
			loggo.Info("[SPP Manager] Higher-priority channel [%s] (priority %d) is healthy; preempting [%s] (priority %d)",
				m.nodes[bestIdx].Name, bestPri, m.nodes[currIdx].Name, currPri)
			_ = m.switchNode(bestIdx)
			return
		}
	}

	// When priorities are equal, maintain stickiness to avoid connection disruption
}

// ReportFailure is called when proxying through the upstream SPP client fails (e.g. connection refused).
func (m *Manager) ReportFailure(err error) {
	if m.closed.Load() {
		return
	}
	currIdx := int(atomic.LoadInt32(&m.activeIndex))
	if currIdx >= 0 && currIdx < len(m.runtimes) {
		fails := m.runtimes[currIdx].consecutiveFails.Add(1)
		m.runtimes[currIdx].consecutiveOks.Store(0)
		if fails >= 5 {
			loggo.Warn("[SPP Manager] Active channel [%s] encountered %d failures (%v), probing liveness",
				m.nodes[currIdx].Name, fails, err)
			go func(idx int) {
				alive, rtt := probeNodeWithRTT(m.nodes[idx], nodeProbeTimeout)
				m.runtimes[idx].alive.Store(alive)
				if !alive {
					loggo.Warn("[SPP Manager] Active channel [%s] probe confirms down, triggering failover", m.nodes[idx].Name)
					select {
					case m.triggerCheckCh <- struct{}{}:
					default:
					}
				} else {
					m.runtimes[idx].rttMs.Store(rtt.Milliseconds())
					m.runtimes[idx].consecutiveFails.Store(0)
				}
			}(currIdx)
		}
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
	Name      string `json:"name"`
	Server    string `json:"server"`
	Proto     string `json:"proto"`
	Priority  int    `json:"priority"`
	Bandwidth int    `json:"bandwidth"`
	RTTMs     int64  `json:"rtt_ms"`
	Alive     bool   `json:"alive"`
	Active    bool   `json:"active"`
}

// NodeStatuses returns status for every configured node.
func (m *Manager) NodeStatuses() []NodeStatus {
	active := m.ActiveNodeIndex()
	out := make([]NodeStatus, 0, len(m.nodes))
	for i, n := range m.nodes {
		rtt := int64(0)
		if i < len(m.runtimes) {
			rtt = m.runtimes[i].rttMs.Load()
		}
		out = append(out, NodeStatus{
			Name:      n.Name,
			Server:    n.Server,
			Proto:     n.ServerProto,
			Priority:  n.getEffectivePriority(),
			Bandwidth: n.getEffectiveBandwidth(),
			RTTMs:     rtt,
			Alive:     m.NodeAlive(i),
			Active:    i == active,
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

// Socks5Addr is the local SOCKS5 address exposed by the active SPP client.
func (m *Manager) Socks5Addr() string {
	idx := m.ActiveNodeIndex()
	if idx >= 0 && idx < len(m.runtimes) {
		return m.runtimes[idx].localSocks5
	}
	return m.localSocks5
}

// Socks5Auth returns empty credentials for local SOCKS5.
func (m *Manager) Socks5Auth() (string, string) {
	return "", ""
}

// Close stops health checks and all active SPP clients.
func (m *Manager) Close() {
	if !m.closed.CompareAndSwap(false, true) {
		return
	}
	close(m.checkStopCh)

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rt := range m.runtimes {
		if rt.client != nil {
			_ = rt.client.Close()
			rt.client = nil
		}
	}
	if m.activeClient != nil {
		_ = m.activeClient.Close()
		m.activeClient = nil
	}
}
