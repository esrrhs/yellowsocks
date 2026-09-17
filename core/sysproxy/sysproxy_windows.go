//go:build windows

package sysproxy

import (
	"fmt"
	"golang.org/x/sys/windows/registry"
)

const internetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// SetGlobalProxy 写入 Windows 注册表设置全局系统代理
func SetGlobalProxy(socks5Addr string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	// 启用系统代理: ProxyEnable = 1
	if err := k.SetDWordValue("ProxyEnable", 1); err != nil {
		return err
	}

	// 设置 Socks5 代理服务器地址: ProxyServer = socks=127.0.0.1:10808
	proxyVal := fmt.Sprintf("socks=%s", socks5Addr)
	if err := k.SetStringValue("ProxyServer", proxyVal); err != nil {
		return err
	}

	// 局域网绕过: ProxyOverride = "<local>;10.*;192.168.*;172.16.*"
	_ = k.SetStringValue("ProxyOverride", "<local>;10.*;192.168.*;172.16.*")

	return nil
}

// SetPACProxy 设置系统 PAC 代理脚本地址
func SetPACProxy(pacURL string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	_ = k.SetDWordValue("ProxyEnable", 0)
	return k.SetStringValue("AutoConfigURL", pacURL)
}

// ClearSystemProxy 清理系统代理
func ClearSystemProxy() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	_ = k.SetDWordValue("ProxyEnable", 0)
	_ = k.DeleteValue("AutoConfigURL")
	return nil
}
