//go:build windows

package netdial

import (
	"math/bits"
	"syscall"

	"golang.org/x/sys/windows"
)

// IP_UNICAST_IF and IPV6_UNICAST_IF share the option number 31 on their
// respective protocol levels. IP_UNICAST_IF expects the interface index in
// network byte order, IPV6_UNICAST_IF in host byte order.
const (
	ipUnicastIf   = 0x1f
	ipv6UnicastIf = 0x1f
)

// bindControl pins sockets to the interface with the given index by setting
// IP_UNICAST_IF / IPV6_UNICAST_IF before connect. Unbound (loopback) dials
// to 127.0.0.1 keep working: the option only steers routing-table lookups
// for new outbound connections.
func bindControl(index uint32, network string, c syscall.RawConn) error {
	return c.Control(func(fd uintptr) {
		_ = bindSocketFDFamily(fd, index, network)
	})
}

// bindSocketFD pins an existing socket (e.g. a ListenUDP socket whose sends
// follow the routing table) to the pinned interface, assuming IPv4.
func bindSocketFD(fd uintptr) error {
	return bindSocketFDFamily(fd, BindInterface(), "udp4")
}

func bindSocketFDFamily(fd uintptr, index uint32, network string) error {
	handle := windows.Handle(fd)
	switch network {
	case "ip4", "tcp4", "udp4":
		return windows.SetsockoptInt(handle, windows.IPPROTO_IP, ipUnicastIf, int(bits.ReverseBytes32(index)))
	case "ip6", "tcp6", "udp6":
		return windows.SetsockoptInt(handle, windows.IPPROTO_IPV6, ipv6UnicastIf, int(index))
	default:
		// "ip", "tcp", "udp": the address family is only known at dial
		// time; apply the IPv4 bind, which is the common case (IPv6
		// destinations dial with an explicit *6 network string).
		return windows.SetsockoptInt(handle, windows.IPPROTO_IP, ipUnicastIf, int(bits.ReverseBytes32(index)))
	}
}
