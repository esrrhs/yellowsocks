//go:build darwin

package guiapp

import (
	"fmt"
	"os/exec"
	"strings"
)

// macOS system proxy is set per network service with networksetup, the same
// command-line path Clash Verge and most proxy clients use.

func networkServices() ([]string, error) {
	out, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return nil, err
	}
	var services []string
	for i, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		// The first line is an explanatory header; disabled services carry
		// a leading asterisk and cannot be configured.
		if i == 0 || line == "" || strings.HasPrefix(line, "*") {
			continue
		}
		services = append(services, line)
	}
	return services, nil
}

func runNetworkSetup(args ...string) error {
	if err := exec.Command("networksetup", args...).Run(); err != nil {
		return fmt.Errorf("networksetup %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func sysproxyEnable(httpPort, socksPort int) error {
	services, err := networkServices()
	if err != nil {
		return err
	}
	for _, svc := range services {
		if httpPort != 0 {
			if err := runNetworkSetup("-setwebproxy", svc, "127.0.0.1", fmt.Sprint(httpPort)); err != nil {
				return err
			}
			if err := runNetworkSetup("-setsecurewebproxy", svc, "127.0.0.1", fmt.Sprint(httpPort)); err != nil {
				return err
			}
		}
		if socksPort != 0 {
			if err := runNetworkSetup("-setsocksfirewallproxy", svc, "127.0.0.1", fmt.Sprint(socksPort)); err != nil {
				return err
			}
		}
		_ = runNetworkSetup("-setproxybypassdomains", svc, "localhost", "127.0.0.1", "192.168.0.0/16", "10.0.0.0/8", "172.16.0.0/12")
	}
	return nil
}

func sysproxyDisable() error {
	services, err := networkServices()
	if err != nil {
		return err
	}
	for _, svc := range services {
		_ = runNetworkSetup("-setwebproxystate", svc, "off")
		_ = runNetworkSetup("-setsecurewebproxystate", svc, "off")
		_ = runNetworkSetup("-setsocksfirewallproxystate", svc, "off")
	}
	return nil
}

// sysproxyProxyServerContains reports whether the first network service's
// web proxy points at 127.0.0.1 on one of the given ports.
func sysproxyProxyServerContains(httpPort, socksPort int) bool {
	services, err := networkServices()
	if err != nil || len(services) == 0 {
		return false
	}
	out, err := exec.Command("networksetup", "-getwebproxy", services[0]).Output()
	if err != nil {
		return false
	}
	text := string(out)
	for _, port := range []int{httpPort, socksPort} {
		if port != 0 && strings.Contains(text, "127.0.0.1") && strings.Contains(text, fmt.Sprint(port)) {
			return true
		}
	}
	return false
}

func sysproxyEnabled() bool {
	services, err := networkServices()
	if err != nil || len(services) == 0 {
		return false
	}
	out, err := exec.Command("networksetup", "-getwebproxy", services[0]).Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "enabled: yes")
}
