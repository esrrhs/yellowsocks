//go:build linux

package tunnel

import (
	"fmt"
	"os/exec"

	"github.com/songgao/water"
)

type linuxTunDevice struct {
	ifce *water.Interface
	name string
}

func (d *linuxTunDevice) Read(b []byte) (int, error) {
	return d.ifce.Read(b)
}

func (d *linuxTunDevice) Write(b []byte) (int, error) {
	return d.ifce.Write(b)
}

func (d *linuxTunDevice) Close() error {
	return d.ifce.Close()
}

func (d *linuxTunDevice) Name() string {
	return d.name
}

// OpenTunDevice 在 Linux 下创建 TUN 网卡并配置 IP
func OpenTunDevice(cfg TunConfig) (Device, error) {
	if cfg.Name == "" {
		cfg.Name = "tun0"
	}
	if cfg.MTU <= 0 {
		cfg.MTU = 1500
	}

	wcfg := water.Config{
		DeviceType: water.TUN,
	}
	wcfg.Name = cfg.Name

	ifce, err := water.New(wcfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create TUN device %s: %w", cfg.Name, err)
	}

	// 使用 ip 命令设置 TUN 网卡 IP 及 MTU
	cmds := [][]string{
		{"ip", "link", "set", "dev", cfg.Name, "mtu", fmt.Sprintf("%d", cfg.MTU)},
		{"ip", "addr", "add", fmt.Sprintf("%s/24", cfg.IP), "dev", cfg.Name},
		{"ip", "link", "set", "dev", cfg.Name, "up"},
	}

	for _, c := range cmds {
		cmd := exec.Command(c[0], c[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			_ = ifce.Close()
			return nil, fmt.Errorf("command %v failed: %s: %w", c, string(out), err)
		}
	}

	return &linuxTunDevice{
		ifce: ifce,
		name: cfg.Name,
	}, nil
}

// SetupGlobalRoutes 配置 Linux 路由让流量流入 TUN 网卡
func SetupGlobalRoutes(tunName, sppServerIP string) error {
	// 确保到 SPP 服务器的路由走默认网关直连，防止流量回环 (routing loop)
	if sppServerIP != "" {
		_ = exec.Command("ip", "route", "add", sppServerIP, "via", "default").Run()
	}

	// 添加两个 /1 路由覆盖整个 IPv4 空间，避免覆盖原有 default 路由
	routes := [][]string{
		{"ip", "route", "add", "0.0.0.0/1", "dev", tunName},
		{"ip", "route", "add", "128.0.0.0/1", "dev", tunName},
	}

	for _, r := range routes {
		_ = exec.Command(r[0], r[1:]...).Run()
	}

	return nil
}

// RestoreGlobalRoutes 清理 Linux 路由
func RestoreGlobalRoutes(tunName, sppServerIP string) error {
	_ = exec.Command("ip", "route", "del", "0.0.0.0/1", "dev", tunName).Run()
	_ = exec.Command("ip", "route", "del", "128.0.0.0/1", "dev", tunName).Run()
	if sppServerIP != "" {
		_ = exec.Command("ip", "route", "del", sppServerIP).Run()
	}
	return nil
}
