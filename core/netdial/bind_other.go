//go:build !windows

package netdial

import "syscall"

// bindControl is a no-op outside Windows: TUN mode currently ships only for
// Windows, and the other platforms have no IP_UNICAST_IF equivalent wired
// into this helper yet.
func bindControl(index uint32, network string, c syscall.RawConn) error {
	return nil
}

// bindSocketFD is a no-op outside Windows; see bindControl.
func bindSocketFD(fd uintptr) error {
	return nil
}
