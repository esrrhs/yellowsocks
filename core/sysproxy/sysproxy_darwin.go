//go:build darwin

package sysproxy

import (
	"bytes"
	"fmt"
	"net"
	"os/exec"
	"strings"
)

// getActiveNetworkServices detects active network service names on macOS (e.g. "Wi-Fi", "Ethernet")
func getActiveNetworkServices() []string {
	out, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return []string{"Wi-Fi", "Ethernet"}
	}

	lines := strings.Split(string(out), "\n")
	var services []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "*") {
			// Lines starting with * are disabled services
			continue
		}
		// Test if service has active IP or hardware port
		services = append(services, trimmed)
	}

	if len(services) == 0 {
		return []string{"Wi-Fi", "Ethernet"}
	}
	return services
}

// SetGlobalProxy configures macOS SOCKS5 proxy using networksetup
func SetGlobalProxy(socks5Addr string) error {
	host, port, err := net.SplitHostPort(socks5Addr)
	if err != nil {
		return fmt.Errorf("invalid socks5 addr %s: %w", socks5Addr, err)
	}

	services := getActiveNetworkServices()
	var lastErr error
	for _, service := range services {
		// Set SOCKS proxy host and port
		cmd := exec.Command("networksetup", "-setsocksfirewallproxy", service, host, port)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			lastErr = fmt.Errorf("failed to set socks proxy on %s: %v (%s)", service, err, stderr.String())
			continue
		}
		// Enable SOCKS proxy state
		_ = exec.Command("networksetup", "-setsocksfirewallproxystate", service, "on").Run()
	}

	return lastErr
}

// SetPACProxy configures macOS PAC proxy URL using networksetup
func SetPACProxy(pacURL string) error {
	services := getActiveNetworkServices()
	var lastErr error
	for _, service := range services {
		cmd := exec.Command("networksetup", "-setautoproxyurl", service, pacURL)
		if err := cmd.Run(); err != nil {
			lastErr = err
			continue
		}
		_ = exec.Command("networksetup", "-setautoproxystate", service, "on").Run()
	}
	return lastErr
}

// ClearSystemProxy disables system proxy on all network services
func ClearSystemProxy() error {
	services := getActiveNetworkServices()
	var lastErr error
	for _, service := range services {
		_ = exec.Command("networksetup", "-setsocksfirewallproxystate", service, "off").Run()
		_ = exec.Command("networksetup", "-setautoproxystate", service, "off").Run()
		_ = exec.Command("networksetup", "-setwebproxystate", service, "off").Run()
		_ = exec.Command("networksetup", "-setsecurewebproxystate", service, "off").Run()
	}
	return lastErr
}
