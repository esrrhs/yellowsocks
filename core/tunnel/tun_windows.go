//go:build windows

package tunnel

import (
	"fmt"
	"os/exec"
	"sync"
	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wintun"
)

type wintunDevice struct {
	adapter *wintun.Adapter
	session wintun.Session
	name    string
	mu      sync.Mutex
	closed  bool
}

func (w *wintunDevice) Read(b []byte) (int, error) {
	packet, err := w.session.ReceivePacket()
	if err != nil {
		return 0, err
	}
	n := copy(b, packet)
	w.session.ReleaseReceivePacket(packet)
	return n, nil
}

func (w *wintunDevice) Write(b []byte) (int, error) {
	packet, err := w.session.AllocateSendPacket(len(b))
	if err != nil {
		return 0, err
	}
	copy(packet, b)
	w.session.SendPacket(packet)
	return len(b), nil
}

func (w *wintunDevice) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	w.session.End()
	return w.adapter.Close()
}

func (w *wintunDevice) Name() string {
	return w.name
}

// OpenTunDevice 在 Windows 下创建 Wintun 适配器
func OpenTunDevice(cfg TunConfig) (Device, error) {
	if cfg.Name == "" {
		cfg.Name = "YellowSocks"
	}

	adapter, err := wintun.CreateAdapter(cfg.Name, "YellowSocks Tunnel", &windows.GUID{})
	if err != nil {
		// 尝试打开已存在的
		adapter, err = wintun.OpenAdapter(cfg.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to create or open Wintun adapter: %w", err)
		}
	}

	session, err := adapter.StartSession(0x800000)
	if err != nil {
		_ = adapter.Close()
		return nil, fmt.Errorf("failed to start wintun session: %w", err)
	}

	// 通过 netsh 配置 IP 地址与掩码
	netshCmd := exec.Command("netsh", "interface", "ipv4", "set", "address",
		fmt.Sprintf("name=\"%s\"", cfg.Name),
		"source=static",
		fmt.Sprintf("addr=%s", cfg.IP),
		fmt.Sprintf("mask=%s", cfg.Mask),
		fmt.Sprintf("gateway=%s", cfg.Gateway),
	)
	_ = netshCmd.Run()

	return &wintunDevice{
		adapter: adapter,
		session: session,
		name:    cfg.Name,
	}, nil
}

// SetupGlobalRoutes Windows 路由设置
func SetupGlobalRoutes(tunName, sppServerIP string) error {
	// 设置 0.0.0.0/1 和 128.0.0.0/1 指向虚拟网卡网关
	_ = exec.Command("route", "add", "0.0.0.0", "mask", "128.0.0.0", "10.255.0.1", "metric", "6").Run()
	_ = exec.Command("route", "add", "128.0.0.0", "mask", "128.0.0.0", "10.255.0.1", "metric", "6").Run()
	return nil
}

// RestoreGlobalRoutes Windows 路由清理
func RestoreGlobalRoutes(tunName, sppServerIP string) error {
	_ = exec.Command("route", "delete", "0.0.0.0", "mask", "128.0.0.0").Run()
	_ = exec.Command("route", "delete", "128.0.0.0", "mask", "128.0.0.0").Run()
	return nil
}
