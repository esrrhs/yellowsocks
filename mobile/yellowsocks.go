package mobile

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/yellowsocks/core"
	"github.com/esrrhs/yellowsocks/core/config"
	"github.com/esrrhs/yellowsocks/core/stats"
)

// Callback Android VpnService 交互回调接口 (供 Kotlin 实现)
type Callback interface {
	OnStatusChanged(running bool, message string)
	OnBandwidthUpdate(uploadBps, downloadBps, totalUpload, totalDownload int64)
	OnActiveNodeChanged(name, server, proto string, latencyMs int64)
}

var (
	engineMu sync.Mutex
	curEngine *core.Engine
	stopStatsCh chan struct{}
)

// StartEngine 启动 Android 系统透明代理引擎
// tunFd: Android VpnService.Builder().establish() 返回的文件描述符
// configContent: 字符串格式的 YAML 或 JSON 配置内容
// cb: 状态与带宽上报回调
func StartEngine(tunFd int, configContent string, cb Callback) error {
	engineMu.Lock()
	defer engineMu.Unlock()

	if curEngine != nil {
		return fmt.Errorf("engine already running")
	}

	if tunFd <= 0 {
		return fmt.Errorf("invalid tun file descriptor: %d", tunFd)
	}

	// 1. 解析传入的配置
	fileCfg, err := config.ParseConfigContent([]byte(configContent))
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	// 2. 初始化核心参数 (移动端由系统 VpnService / NetworkExtension 自动路由接管)
	baseCfg := core.EngineConfig{
		TunFd:        tunFd,
		TunName:      "tun-mobile",
		TunIP:        "10.255.0.2",
		TunGateway:   "10.255.0.1",
		TunMask:      "255.255.255.0",
		MTU:          1500,
		EnableFakeIP: true,
		DNSListen:    "127.0.0.1:53",
		DirectDNS:    "1.1.1.1:53",
		RemoteDoH:    "https://1.1.1.1/dns-query",
		SetAutoRoute: false, // 移动端由系统层自动注入路由，无需执行宿主系统命令行
	}

	cfg := fileCfg.MergeWithEngineConfig(baseCfg)
	cfg.TunFd = tunFd
	cfg.SetAutoRoute = false // 确保移动端不执行宿主系统命令

	// 3. 配置日志系统
	logLevel := loggo.LEVEL_INFO
	if fileCfg.LogLevel != "" && loggo.NameToLevel(fileCfg.LogLevel) >= 0 {
		logLevel = loggo.NameToLevel(fileCfg.LogLevel)
	}
	loggo.Ini(loggo.Config{
		Level:  logLevel,
		Prefix: "yellowsocks-mobile",
	})

	loggo.Info("[Mobile] Starting YellowSocks Engine with tunFd=%d...", tunFd)

	eng := core.NewEngine(cfg)
	if err := eng.Start(); err != nil {
		loggo.Error("[Mobile] Failed to start engine: %v", err)
		if cb != nil {
			cb.OnStatusChanged(false, err.Error())
		}
		return err
	}

	curEngine = eng

	if cb != nil {
		cb.OnStatusChanged(true, "Connected")
	}

	// 4. 开启定时速率与节点状态上报
	stopStatsCh = make(chan struct{})
	go startStatsTicker(cb, stopStatsCh)

	return nil
}

// StopEngine 停止 Android 代理内核
func StopEngine() error {
	engineMu.Lock()
	defer engineMu.Unlock()

	if curEngine == nil {
		return nil
	}

	if stopStatsCh != nil {
		close(stopStatsCh)
		stopStatsCh = nil
	}

	err := curEngine.Stop()
	curEngine = nil
	return err
}

// IsRunning 检查当前代理状态
func IsRunning() bool {
	engineMu.Lock()
	defer engineMu.Unlock()
	return curEngine != nil
}

// SwitchNode 手动切换 SPP 节点
func SwitchNode(index int) error {
	engineMu.Lock()
	defer engineMu.Unlock()

	if curEngine == nil || curEngine.SPPManager() == nil {
		return fmt.Errorf("engine not running or no SPP manager")
	}
	return curEngine.SPPManager().SwitchToNode(index)
}

// GetStatsSnapshotJSON 获取当前详细网络与连接快照 (JSON 格式)
func GetStatsSnapshotJSON() string {
	snapshot := stats.Default.GetSnapshot()
	data, err := json.Marshal(snapshot)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// GetNodesJSON 获取节点列表及当前活跃节点状态
func GetNodesJSON() string {
	engineMu.Lock()
	defer engineMu.Unlock()

	if curEngine == nil || curEngine.SPPManager() == nil {
		return "[]"
	}
	nodes := curEngine.SPPManager().GetAllNodes()
	data, err := json.Marshal(nodes)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func startStatsTicker(cb Callback, stopCh chan struct{}) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			snapshot := stats.Default.GetSnapshot()
			if cb != nil {
				cb.OnBandwidthUpdate(
					snapshot.UploadSpeed,
					snapshot.DownloadSpeed,
					snapshot.TotalUpload,
					snapshot.TotalDownload,
				)

				engineMu.Lock()
				if curEngine != nil && curEngine.SPPManager() != nil {
					activeNode := curEngine.SPPManager().ActiveNode()
					if activeNode != nil {
						cb.OnActiveNodeChanged(
							activeNode.Name,
							activeNode.Server,
							activeNode.ServerProto,
							activeNode.Latency.Milliseconds(),
						)
					}
				}
				engineMu.Unlock()
			}
		}
	}
}
