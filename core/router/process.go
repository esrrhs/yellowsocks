package router

import (
	"path/filepath"
	"strings"
	"sync"
	"time"

	psnet "github.com/shirou/gopsutil/v3/net"
	psproc "github.com/shirou/gopsutil/v3/process"
)

// ProcessInspector 负责维护本地端口与进程名/可执行文件的对应关系
type ProcessInspector struct {
	bypassApps  sync.Map // appName (lowercase) -> bool
	portToProc  sync.Map // port (uint32) -> procName
	stopCh      chan struct{}
}

// NewProcessInspector 创建进程检查器
func NewProcessInspector() *ProcessInspector {
	pi := &ProcessInspector{
		stopCh: make(chan struct{}),
	}
	// 默认排除常见高并发、游戏或内部应用
	defaultBypass := []string{
		"dota2.exe",
		"dota2",
		"thunder.exe",
		"xunlei.exe",
		"baidunetdisk.exe",
		"cloudmusic.exe",
		"yellowsocks-cli",
		"yellowsocks-gui.exe",
	}
	for _, app := range defaultBypass {
		pi.AddBypassApp(app)
	}

	// 启动后台轮询，更新端口与进程名的映射
	go pi.startPolling(2 * time.Second)

	return pi
}

// AddBypassApp 添加需排除直连的应用名 (如 thunder.exe 或 baidunetdisk)
func (pi *ProcessInspector) AddBypassApp(appName string) {
	appName = strings.ToLower(filepath.Base(appName))
	pi.bypassApps.Store(appName, true)
}

// RemoveBypassApp 移除排除应用
func (pi *ProcessInspector) RemoveBypassApp(appName string) {
	appName = strings.ToLower(filepath.Base(appName))
	pi.bypassApps.Delete(appName)
}

// IsBypassPort 检查该源端口对应的进程是否在排除白名单中
func (pi *ProcessInspector) IsBypassPort(srcPort int) bool {
	val, ok := pi.portToProc.Load(uint32(srcPort))
	if !ok {
		return false
	}
	procName := val.(string)
	_, bypassed := pi.bypassApps.Load(procName)
	return bypassed
}

// GetProcessByPort 查询源端口对应进程名
func (pi *ProcessInspector) GetProcessByPort(srcPort int) (string, bool) {
	val, ok := pi.portToProc.Load(uint32(srcPort))
	if !ok {
		return "", false
	}
	return val.(string), true
}

func (pi *ProcessInspector) startPolling(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// 预先执行一次
	pi.refreshConnections()

	for {
		select {
		case <-pi.stopCh:
			return
		case <-ticker.C:
			pi.refreshConnections()
		}
	}
}

func (pi *ProcessInspector) refreshConnections() {
	conns, err := psnet.Connections("tcp")
	if err != nil {
		return
	}

	// 缓存已查询过的 pid -> name，减少系统开销
	pidCache := make(map[int32]string)

	for _, c := range conns {
		if c.Pid <= 0 || c.Laddr.Port == 0 {
			continue
		}

		name, ok := pidCache[c.Pid]
		if !ok {
			if proc, err := psproc.NewProcess(c.Pid); err == nil {
				if pname, err := proc.Name(); err == nil {
					name = strings.ToLower(pname)
					pidCache[c.Pid] = name
				}
			}
		}

		if name != "" {
			pi.portToProc.Store(c.Laddr.Port, name)
		}
	}
}

// Close 关闭检查器
func (pi *ProcessInspector) Close() {
	select {
	case <-pi.stopCh:
	default:
		close(pi.stopCh)
	}
}
