package main

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/esrrhs/gohome/loggo"
)

// System proxy management, in the spirit of Clash Verge's "system proxy"
// toggle: the app registers its inbound HTTP and SOCKS5 ports as the OS-wide
// proxy, and removes them again on disable/quit. Platform implementations
// live in sysproxy_windows.go and sysproxy_darwin.go.

// enableSystemProxy points the OS at the given local proxy ports. httpAddr
// may be empty when only the SOCKS5 inbound is configured.
func enableSystemProxy(httpAddr, socksAddr string) error {
	httpPort, socksPort, err := proxyPorts(httpAddr, socksAddr)
	if err != nil {
		return err
	}
	return sysproxyEnable(httpPort, socksPort)
}

// disableSystemProxy removes the OS-wide proxy registration.
func disableSystemProxy() error {
	return sysproxyDisable()
}

// systemProxyEnabled reports whether the OS currently has a proxy enabled.
// It is best-effort; callers keep their own desired state and only use this
// to sanity-check.
func systemProxyEnabled() bool {
	return sysproxyEnabled()
}

func proxyPorts(httpAddr, socksAddr string) (httpPort int, socksPort int, err error) {
	portOf := func(addr string) int {
		if addr == "" {
			return 0
		}
		_, p, e := net.SplitHostPort(addr)
		if e != nil {
			return 0
		}
		v, e := strconv.Atoi(p)
		if e != nil {
			return 0
		}
		return v
	}
	httpPort = portOf(httpAddr)
	socksPort = portOf(socksAddr)
	if httpPort == 0 && socksPort == 0 {
		return 0, 0, errors.New("no local proxy port to register as system proxy")
	}
	return httpPort, socksPort, nil
}

// proxyBypassList is the set of hosts that must never go through the proxy:
// loopback, link-local and RFC1918 ranges plus Windows' <local> marker.
func proxyBypassList() string {
	return "localhost;127.*;10.*;172.16.*;172.17.*;172.18.*;172.19.*;" +
		"172.20.*;172.21.*;172.22.*;172.23.*;172.24.*;172.25.*;172.26.*;" +
		"172.27.*;172.28.*;172.29.*;172.30.*;172.31.*;192.168.*;<local>"
}

func formatProxyPorts(httpPort, socksPort int) string {
	parts := []string{}
	if httpPort != 0 {
		p := strconv.Itoa(httpPort)
		parts = append(parts, "http=127.0.0.1:"+p, "https=127.0.0.1:"+p)
	}
	if socksPort != 0 {
		p := strconv.Itoa(socksPort)
		parts = append(parts, "socks=127.0.0.1:"+p)
	}
	return fmt.Sprintf("%s", joinSemicolon(parts))
}

func joinSemicolon(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ";"
		}
		out += p
	}
	return out
}

// cleanupStaleSystemProxy removes a system proxy registration left behind by
// a hard-killed previous instance. It must run before wails.Run: a stale
// proxy pointing at a dead port blocks the WebView2 subprocess itself from
// starting, which would otherwise prevent the app (and the cleanup) from
// ever running.
func cleanupStaleSystemProxy() {
	st, err := loadSettings()
	if err != nil || st.AppliedPorts == "" {
		return
	}
	if !sysproxyPointsAt(st.AppliedPorts) {
		return
	}
	loggo.Info("[GUI] clearing stale system proxy from previous run")
	if err := disableSystemProxy(); err == nil {
		// Drop the record; a later enable re-registers from scratch.
		st.AppliedPorts = ""
		_ = saveSettings(st)
	}
}

// sysproxyPointsAt reports whether the OS-wide proxy currently points at the
// given ports ("http;socks" form). Used to detect a registration left behind
// by a hard-killed previous instance.
func sysproxyPointsAt(ports string) bool {
	parts := strings.Split(ports, ";")
	if len(parts) != 2 {
		return false
	}
	httpPort, err1 := strconv.Atoi(parts[0])
	socksPort, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return sysproxyProxyServerContains(httpPort, socksPort)
}
