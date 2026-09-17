//go:build !windows && !darwin

package sysproxy

// SetGlobalProxy Linux/Unix fallback
func SetGlobalProxy(socks5Addr string) error {
	return nil
}

// SetPACProxy Linux/Unix fallback
func SetPACProxy(pacURL string) error {
	return nil
}

// ClearSystemProxy Linux/Unix fallback
func ClearSystemProxy() error {
	return nil
}
