//go:build windows

package guiapp

import (
	"fmt"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

const internetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

var (
	wininet               = syscall.NewLazyDLL("wininet.dll")
	procInternetSetOption = wininet.NewProc("InternetSetOptionW")
)

const (
	internetOptionSettingsChanged = 39
	internetOptionRefresh         = 37
)

// refreshWinINET makes running applications notice the registry change
// without a reboot or re-login (same trick Clash Verge's Rust side uses).
func refreshWinINET() {
	procInternetSetOption.Call(0, internetOptionSettingsChanged, 0, 0)
	procInternetSetOption.Call(0, internetOptionRefresh, 0, 0)
}

func sysproxyEnable(httpPort, socksPort int) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open internet settings: %w", err)
	}
	defer key.Close()

	if err := key.SetDWordValue("ProxyEnable", 1); err != nil {
		return err
	}
	if err := key.SetStringValue("ProxyServer", formatProxyPorts(httpPort, socksPort)); err != nil {
		return err
	}
	if err := key.SetStringValue("ProxyOverride", proxyBypassList()); err != nil {
		return err
	}
	refreshWinINET()
	return nil
}

func sysproxyDisable() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open internet settings: %w", err)
	}
	defer key.Close()

	if err := key.SetDWordValue("ProxyEnable", 0); err != nil {
		return err
	}
	refreshWinINET()
	return nil
}

// sysproxyProxyServerContains reports whether the registered ProxyServer
// mentions 127.0.0.1 on one of the given ports.
func sysproxyProxyServerContains(httpPort, socksPort int) bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	v, _, err := key.GetStringValue("ProxyServer")
	if err != nil {
		return false
	}
	for _, port := range []int{httpPort, socksPort} {
		if port != 0 && strings.Contains(v, "127.0.0.1:"+strconv.Itoa(port)) {
			return true
		}
	}
	return false
}

func sysproxyEnabled() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	v, _, err := key.GetIntegerValue("ProxyEnable")
	return err == nil && v == 1
}
