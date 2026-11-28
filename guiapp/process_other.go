//go:build !windows

package guiapp

import (
	"net"
)

// Process attribution is Windows-only: the connections page falls back to
// the raw client address on platforms without a resolver.
type processResolver struct {
	tunActive func() bool // API parity with the Windows resolver; unused here
}

func newProcessResolver(tcpListenPorts []string, tunActive func() bool) *processResolver {
	return &processResolver{tunActive: tunActive}
}

func (*processResolver) UpdateListenPorts(addrs []string) {}

func (*processResolver) FindProcess(srcIP net.IP, srcPort int, dstHost string, dstPort int) (string, string) {
	return "", ""
}
