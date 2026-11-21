//go:build !windows && !darwin

package main

import "errors"

// Other platforms have no system proxy support in this milestone.

func sysproxyEnable(httpPort, socksPort int) error {
	return errors.New("system proxy is not supported on this platform")
}

func sysproxyDisable() error {
	return errors.New("system proxy is not supported on this platform")
}

func sysproxyProxyServerContains(httpPort, socksPort int) bool { return false }

func sysproxyEnabled() bool { return false }
