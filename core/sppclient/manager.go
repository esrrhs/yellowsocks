package sppclient

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/esrrhs/gohome/loggo"
)

// Node is one upstream SPP server.
type Node struct {
	Name        string `json:"name"`
	Server      string `json:"server"`
	ServerProto string `json:"server_proto"` // tcp, udp, rudp, kcp, quic
	Key         string `json:"key"`
	Encrypt     string `json:"encrypt"`
	Compress    int    `json:"compress"`
	// Alive is a configuration/status hint carried in JSON. Runtime liveness
	// is tracked atomically inside Manager (see Manager.NodeAlive); this field
	// is never read by failover logic.
	Alive bool `json:"alive"`
}

const (
	defaultHealthCheckInterval = 15 * time.Second
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

// Manager keeps the SPP node list, exposes one local SOCKS5, and fails over.
type Manager struct {
	nodes        []*Node
	alive        []atomic.Bool // per-node latest probe result
	activeClient *Client
	activeIndex  int32 // -1 while no node is active
	localSocks5  string
	checkStopCh  chan struct{}
	// closed flips before checkStopCh closes, so a probe sweep finishing
	// during shutdown cannot rebuild a client that Close just released.
	closed         atomic.Bool
	healthInterval time.Duration
	mu             sync.Mutex
}

// NewManager starts the first reachable SPP node and a local SOCKS5 for DNS
// and inbound proxies. A background health check fails over when the active
// node stops answering.
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

	m := &Manager{
		nodes:          nodes,
		alive:          make([]atomic.Bool, len(nodes)),
		localSocks5:    localSocks5,
		checkStopCh:    make(chan struct{}),
		activeIndex:    -1,
		healthInterval: interval,
	}

	// spp's NewClient never dials synchronously: it starts a background loop
	// that retries forever, so constructing a client against a dead TCP port
	// "succeeds". Probe candidates ourselves and start on the first reachable
	// one; otherwise a dead primary would carry traffic until the first
	// health-check tick (up to healthInterval later).
	idx := m.firstReachable()
	if idx < 0 {
		loggo.Error("[SPP Manager] All %d SPP nodes are unreachable at startup; starting node 0 anyway, health check will fail over when one recovers", len(nodes))
		idx = 0
	}
	if err := m.switchNode(idx); err != nil {
		loggo.Warn("[SPP Manager] Initial active node %d [%s] failed to start: %v, trying fallback", idx, m.nodes[idx].Name, err)
		if fb := m.fallbackLocked(idx); fb < 0 {
			loggo.Error("[SPP Manager] No SPP node could be started")
		}
	}

	go m.healthLoop()
	return m, nil
}

// probeNode dials the node once. For TCP the handshake proves the server is
// accepting connections. For UDP-family protocols a dial only validates the
// address/route (UDP connect performs no handshake), so it is best-effort.
func probeNode(node *Node) bool {
	conn, err := net.DialTimeout(dialNetworkForProto(node.ServerProto), node.Server, nodeProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// firstReachable probes nodes in configured order and returns the first alive
// index, recording every probe result in m.alive. Returns -1 if all fail.
func (m *Manager) firstReachable() int {
	for i, node := range m.nodes {
		alive := probeNode(node)
		m.alive[i].Store(alive)
		if alive {
			return i
		}
	}
	return -1
}

// switchNode stops the active client and starts the one at index. The caller
// must hold m.mu (or be the constructor before the health loop starts).
func (m *Manager) switchNode(index int) error {
	if m.closed.Load() {
		return fmt.Errorf("spp manager is closed")
	}
	if index < 0 || index >= len(m.nodes) {
		return fmt.Errorf("node index out of range")
	}

	target := m.nodes[index]
	loggo.Info("[SPP Manager] Switching active node to [%s] (%s via %s)...", target.Name, target.Server, target.ServerProto)

	if m.activeClient != nil {
		_ = m.activeClient.Close()
		m.activeClient = nil
	}

	cfg := &Config{
		ServerProto: target.ServerProto,
		Server:      target.Server,
		Key:         target.Key,
		Encrypt:     target.Encrypt,
		Compress:    target.Compress,
		LocalSocks5: m.localSocks5,
		Name:        target.Name,
	}

	cl, err := NewClient(cfg)
	if err != nil {
		m.alive[index].Store(false)
		return err
	}

	m.alive[index].Store(true)
	m.activeClient = cl
	atomic.StoreInt32(&m.activeIndex, int32(index))
	loggo.Info("[SPP Manager] Successfully connected to [%s]", target.Name)
	return nil
}

// fallbackLocked probes every other node and switches to the first one that
// answers. exclude is an index known to have just failed (-1 to allow all).
// Returns the new active index, or -1 when every candidate failed. The caller
// must hold m.mu (or be the constructor).
func (m *Manager) fallbackLocked(exclude int) int {
	curr := int(atomic.LoadInt32(&m.activeIndex))
	for i, node := range m.nodes {
		if i == exclude {
			continue
		}
		if i == curr && m.activeClient != nil {
			continue
		}
		// Re-probe: sweep results can already be stale, and UDP-family
		// probes are best-effort.
		if !probeNode(node) {
			m.alive[i].Store(false)
			continue
		}
		if err := m.switchNode(i); err == nil {
			loggo.Info("[SPP Manager] Fallback succeeded to node [%s]", node.Name)
			return i
		}
	}
	loggo.Error("[SPP Manager] All SPP nodes are unreachable!")
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
		}
	}
}

// checkNodes probes every node concurrently and fails over when the active
// node is down.
func (m *Manager) checkNodes() {
	var wg sync.WaitGroup
	for i := range m.nodes {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			m.alive[idx].Store(probeNode(m.nodes[idx]))
		}(i)
	}
	wg.Wait()

	if m.closed.Load() {
		return
	}
	currIdx := int(atomic.LoadInt32(&m.activeIndex))
	if currIdx >= 0 && currIdx < len(m.nodes) && m.alive[currIdx].Load() {
		return
	}
	loggo.Warn("[SPP Manager] Current node is down, initiating automatic failover...")

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed.Load() {
		return
	}
	// The state may have changed while waiting for the lock.
	currIdx = int(atomic.LoadInt32(&m.activeIndex))
	if currIdx >= 0 && currIdx < len(m.nodes) && m.alive[currIdx].Load() {
		return
	}
	m.fallbackLocked(currIdx)
}

// ActiveNodeIndex returns the index of the currently active node, -1 if none.
func (m *Manager) ActiveNodeIndex() int {
	return int(atomic.LoadInt32(&m.activeIndex))
}

// NodeAlive reports the latest probe result for the given node index.
func (m *Manager) NodeAlive(index int) bool {
	if index < 0 || index >= len(m.alive) {
		return false
	}
	return m.alive[index].Load()
}

// Socks5Addr is the local SOCKS5 address exposed by the active SPP client.
func (m *Manager) Socks5Addr() string {
	return m.localSocks5
}

// Socks5Auth is empty. The local SPP SOCKS5 does not require a username.
func (m *Manager) Socks5Auth() (string, string) {
	return "", ""
}

// Close stops health checks and the active SPP client. It is idempotent; a
// health-check sweep finishing concurrently cannot start a replacement client
// after Close returns.
func (m *Manager) Close() {
	if !m.closed.CompareAndSwap(false, true) {
		return
	}
	close(m.checkStopCh)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeClient != nil {
		_ = m.activeClient.Close()
		m.activeClient = nil
	}
}
