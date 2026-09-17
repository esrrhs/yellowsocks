package stats

import (
	"sync"
	"sync/atomic"
	"time"
)

// ConnectionRecord 活跃连接记录
type ConnectionRecord struct {
	ID        string    `json:"id"`
	Process   string    `json:"process"`
	Source    string    `json:"source"`
	Target    string    `json:"target"`
	Domain    string    `json:"domain"`
	Rule      string    `json:"rule"` // Direct or Proxy (SPP)
	Upload    int64     `json:"upload"`
	Download  int64     `json:"download"`
	StartTime time.Time `json:"start_time"`
}

// Manager 全局流量统计与连接监控器
type Manager struct {
	uploadSpeed   int64
	downloadSpeed int64
	totalUpload   int64
	totalDownload int64

	periodUpload   int64
	periodDownload int64

	mu          sync.RWMutex
	connections map[string]*ConnectionRecord
	recentLogs  []string
	maxLogs     int
	stopCh      chan struct{}
}

var Default = NewManager()

func NewManager() *Manager {
	m := &Manager{
		connections: make(map[string]*ConnectionRecord),
		recentLogs:  make([]string, 0),
		maxLogs:     200,
		stopCh:      make(chan struct{}),
	}
	go m.speedCalculator()
	return m
}

func (m *Manager) speedCalculator() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			up := atomic.SwapInt64(&m.periodUpload, 0)
			down := atomic.SwapInt64(&m.periodDownload, 0)
			atomic.StoreInt64(&m.uploadSpeed, up)
			atomic.StoreInt64(&m.downloadSpeed, down)
		}
	}
}

// AddTraffic 累加流量
func (m *Manager) AddTraffic(upload, download int64) {
	if upload > 0 {
		atomic.AddInt64(&m.totalUpload, upload)
		atomic.AddInt64(&m.periodUpload, upload)
	}
	if download > 0 {
		atomic.AddInt64(&m.totalDownload, download)
		atomic.AddInt64(&m.periodDownload, download)
	}
}

// TrackConnection 记录新连接
func (m *Manager) TrackConnection(id, proc, src, target, domain, rule string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.connections[id] = &ConnectionRecord{
		ID:        id,
		Process:   proc,
		Source:    src,
		Target:    target,
		Domain:    domain,
		Rule:      rule,
		StartTime: time.Now(),
	}
}

// UpdateConnectionTraffic 更新特定连接的流量
func (m *Manager) UpdateConnectionTraffic(id string, upload, download int64) {
	m.AddTraffic(upload, download)

	m.mu.Lock()
	defer m.mu.Unlock()
	if conn, ok := m.connections[id]; ok {
		conn.Upload += upload
		conn.Download += download
	}
}

// RemoveConnection 移除已结束的连接
func (m *Manager) RemoveConnection(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.connections, id)
}

// AddLog 添加最近日志
func (m *Manager) AddLog(log string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recentLogs = append(m.recentLogs, time.Now().Format("15:04:05")+" "+log)
	if len(m.recentLogs) > m.maxLogs {
		m.recentLogs = m.recentLogs[len(m.recentLogs)-m.maxLogs:]
	}
}

// Snapshot 导出监控快照
type Snapshot struct {
	UploadSpeed   int64               `json:"upload_speed"`
	DownloadSpeed int64               `json:"download_speed"`
	TotalUpload   int64               `json:"total_upload"`
	TotalDownload int64               `json:"total_download"`
	ActiveConns   []*ConnectionRecord `json:"active_connections"`
	Logs          []string            `json:"recent_logs"`
}

func (m *Manager) GetSnapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	conns := make([]*ConnectionRecord, 0, len(m.connections))
	for _, c := range m.connections {
		conns = append(conns, c)
	}

	logs := make([]string, len(m.recentLogs))
	copy(logs, m.recentLogs)

	return Snapshot{
		UploadSpeed:   atomic.LoadInt64(&m.uploadSpeed),
		DownloadSpeed: atomic.LoadInt64(&m.downloadSpeed),
		TotalUpload:   atomic.LoadInt64(&m.totalUpload),
		TotalDownload: atomic.LoadInt64(&m.totalDownload),
		ActiveConns:   conns,
		Logs:          logs,
	}
}

func (m *Manager) Close() {
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
}
