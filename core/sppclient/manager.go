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

// Node SPP 节点定义
type Node struct {
	Name        string        `json:"name"`
	Server      string        `json:"server"` // ip:port
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

// Manager 维护多 SPP 节点池、心跳探测与自动故障转移
type Manager struct {
	nodes        []*Node
	activeClient *Client
	activeIndex  int32
	localSocks5  string
	checkStopCh  chan struct{}
	mu           sync.RWMutex
}

// NewManager 初始化多节点管理器
func NewManager(nodes []*Node, localSocks5 string) (*Manager, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no spp nodes provided")
	}
	if localSocks5 == "" {
		localSocks5 = "127.0.0.1:10808"
	}

	m := &Manager{
		nodes:        nodes,
		localSocks5:  localSocks5,
		checkStopCh:  make(chan struct{}),
		activeIndex:  0,
	}

	// 启动首个活跃节点
	if err := m.switchNode(0); err != nil {
		loggo.Warn("[SPP Manager] Initial active node 0 connect failed: %v, will try fallback", err)
		m.tryFallback()
	}

	// 开启后台心跳检测与健康检查
	go m.startHealthCheck(15 * time.Second)

	return m, nil
}

func (m *Manager) switchNode(index int) error {
	if index < 0 || index >= len(m.nodes) {
		return fmt.Errorf("node index out of range")
	}

	target := m.nodes[index]
	loggo.Info("[SPP Manager] Switching active node to [%s] (%s via %s)...", target.Name, target.Server, target.ServerProto)

	// 关闭旧客户端
	if m.activeClient != nil {
		_ = m.activeClient.Close()
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
		if i == int(atomic.LoadInt32(&m.activeIndex)) {
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

	// 检查当前节点是否存活，若已挂断则触发故障转移
	currIdx := int(atomic.LoadInt32(&m.activeIndex))
	if !m.nodes[currIdx].Alive {
		loggo.Warn("[SPP Manager] Current node [%s] is down, initiating automatic failover...", m.nodes[currIdx].Name)
		m.mu.Lock()
		m.tryFallback()
		m.mu.Unlock()
	}
}

// Socks5Addr 当前活跃节点的本地 Socks5 地址
func (m *Manager) Socks5Addr() string {
	return m.localSocks5
}

// ActiveNode 返回当前正在使用的节点信息
func (m *Manager) ActiveNode() *Node {
	idx := atomic.LoadInt32(&m.activeIndex)
	return m.nodes[idx]
}

// GetAllNodes 获取所有节点状态列表
func (m *Manager) GetAllNodes() []*Node {
	return m.nodes
}

// SwitchToNode 手动切换指定节点
func (m *Manager) SwitchToNode(index int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.switchNode(index)
}

// Close 关闭管理器
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
	}
}
