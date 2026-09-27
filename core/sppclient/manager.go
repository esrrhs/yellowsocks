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
	Name        string        `json:"name"`
	Server      string        `json:"server"`
	ServerProto string        `json:"server_proto"` // tcp, udp, rudp, kcp, quic
	Key         string        `json:"key"`
	Encrypt     string        `json:"encrypt"`
	Compress    int           `json:"compress"`
	Latency     time.Duration `json:"latency"`
	Alive       bool          `json:"alive"`
}

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
	activeClient *Client
	activeIndex  int32
	localSocks5  string
	checkStopCh  chan struct{}
	mu           sync.RWMutex
}

// NewManager starts the first SPP node and a local SOCKS5 for DNS and inbound proxies.
func NewManager(nodes []*Node, localSocks5 string) (*Manager, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no spp nodes provided")
	}
	if localSocks5 == "" {
		localSocks5 = "127.0.0.1:10808"
	}

	m := &Manager{
		nodes:       nodes,
		localSocks5: localSocks5,
		checkStopCh: make(chan struct{}),
		activeIndex: 0,
	}

	if err := m.switchNode(0); err != nil {
		loggo.Warn("[SPP Manager] Initial active node 0 connect failed: %v, will try fallback", err)
		m.tryFallback()
	}

	go m.startHealthCheck(15 * time.Second)
	return m, nil
}

func (m *Manager) switchNode(index int) error {
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
		target.Alive = false
		return err
	}

	target.Alive = true
	m.activeClient = cl
	atomic.StoreInt32(&m.activeIndex, int32(index))
	loggo.Info("[SPP Manager] Successfully connected to [%s]", target.Name)
	return nil
}

func (m *Manager) tryFallback() {
	for i, node := range m.nodes {
		if i == int(atomic.LoadInt32(&m.activeIndex)) && m.activeClient != nil {
			continue
		}
		if err := m.switchNode(i); err == nil {
			loggo.Info("[SPP Manager] Fallback succeeded to node [%s]", node.Name)
			return
		}
	}
	loggo.Error("[SPP Manager] All SPP nodes are unreachable!")
}

func (m *Manager) startHealthCheck(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.checkStopCh:
			return
		case <-ticker.C:
			m.checkAllNodes()
		}
	}
}

func (m *Manager) checkAllNodes() {
	var wg sync.WaitGroup
	for i := range m.nodes {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			node := m.nodes[idx]
			network := dialNetworkForProto(node.ServerProto)
			start := time.Now()
			conn, err := net.DialTimeout(network, node.Server, 3*time.Second)
			if err != nil {
				node.Alive = false
				node.Latency = 0
				return
			}
			node.Alive = true
			node.Latency = time.Since(start)
			conn.Close()
		}(i)
	}
	wg.Wait()

	currIdx := int(atomic.LoadInt32(&m.activeIndex))
	if currIdx < 0 || currIdx >= len(m.nodes) || m.nodes[currIdx].Alive {
		return
	}
	loggo.Warn("[SPP Manager] Current node [%s] is down, initiating automatic failover...", m.nodes[currIdx].Name)
	m.mu.Lock()
	m.tryFallback()
	m.mu.Unlock()
}

// Socks5Addr is the local SOCKS5 address exposed by the active SPP client.
func (m *Manager) Socks5Addr() string {
	return m.localSocks5
}

// Socks5Auth is empty. The local SPP SOCKS5 does not require a username.
func (m *Manager) Socks5Auth() (string, string) {
	return "", ""
}

// ActiveNode returns the node currently used for proxied traffic.
func (m *Manager) ActiveNode() *Node {
	idx := atomic.LoadInt32(&m.activeIndex)
	if idx < 0 || int(idx) >= len(m.nodes) {
		return nil
	}
	return m.nodes[idx]
}

// GetAllNodes returns every configured node.
func (m *Manager) GetAllNodes() []*Node {
	return m.nodes
}

// SwitchToNode selects a node.
func (m *Manager) SwitchToNode(index int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.switchNode(index)
}

// Close stops health checks and the active SPP client.
func (m *Manager) Close() {
	select {
	case <-m.checkStopCh:
	default:
		close(m.checkStopCh)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeClient != nil {
		_ = m.activeClient.Close()
		m.activeClient = nil
	}
}
