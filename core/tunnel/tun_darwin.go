//go:build darwin

package tunnel

import (
	"os/exec"
)

// SetupGlobalRoutes 配置 macOS 路由让流量流入 TUN 网卡
func SetupGlobalRoutes(tunName, sppServerIP string) error {
	// 确保到 SPP 服务器的路由走原默认网关，防止流量回环
	if sppServerIP != "" {
		_ = exec.Command("route", "add", "-host", sppServerIP, "-interface", "en0").Run()
	}

	// macOS 通过 route add 添加覆盖全网的路由指向 tun 网卡
	_ = exec.Command("route", "add", "-net", "0.0.0.0/1", "-interface", tunName).Run()
	_ = exec.Command("route", "add", "-net", "128.0.0.0/1", "-interface", tunName).Run()
	return nil
}

// RestoreGlobalRoutes 清理 macOS 路由
func RestoreGlobalRoutes(tunName, sppServerIP string) error {
	_ = exec.Command("route", "delete", "-net", "0.0.0.0/1", "-interface", tunName).Run()
	_ = exec.Command("route", "delete", "-net", "128.0.0.0/1", "-interface", tunName).Run()
	if sppServerIP != "" {
		_ = exec.Command("route", "delete", "-host", sppServerIP).Run()
	}
	return nil
}
