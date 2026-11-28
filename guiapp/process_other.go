//go:build !windows

package guiapp

import (
	"net"

	"github.com/esrrhs/yellowsocks/core/proxy"
)

// Process attribution is Windows-only: the connections page falls back to
// the raw client address on platforms without a resolver.
type processResolver struct{}

func newProcessResolver(tcpListenPorts []string, tunActive func() bool) processResolver {
	return processResolver{}
}

func (processResolver) UpdateListenPorts(addrs []string) {}

func (processResolver) FindProcess(srcIP net.IP, srcPort int, dstHost string, dstPort int) (string, string) {
	return "", ""
}
