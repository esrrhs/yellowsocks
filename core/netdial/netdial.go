// Package netdial centralizes every outbound dial the engine makes (direct
// relays, upstream connections, DNS upstreams) behind one place, so the GUI
// can pin all of them to the physical network interface while TUN mode is
// active. Without the bind, engine traffic would be routed back into the
// virtual adapter by the TUN default routes and loop forever.
//
// The bind is a process-wide decision: GUI calls SetBindInterface when TUN
// goes up and SetBindInterface(0) when it goes down. A nil/0 bind leaves
// every dialer exactly as before, so CLI and non-TUN GUI behave identically.
package netdial

import (
	"fmt"
	"net"
	"sync/atomic"
	"syscall"
	"time"
)

// bindIndex holds the physical interface index sockets are pinned to, or 0
// for "no bind" (the default everywhere except TUN mode).
var bindIndex atomic.Uint32

// SetBindInterface pins future dials to the interface with the given index.
// 0 clears the pin.
func SetBindInterface(index uint32) { bindIndex.Store(index) }

// BindInterface reports the currently pinned interface index (0 = none).
func BindInterface() uint32 { return bindIndex.Load() }

// Control is the Dialer.Control callback implementing the bind. Callers may
// wrap it: the wrapped control must eventually call this one.
func Control(network, address string, c syscall.RawConn) error {
	idx := BindInterface()
	if idx == 0 {
		return nil
	}
	return bindControl(idx, network, c)
}

// WrapDialer returns d with the bind control installed.
func WrapDialer(d *net.Dialer) *net.Dialer {
	d.Control = Control
	return d
}

// DialTimeout dials network/address with the bind control applied.
func DialTimeout(network, addr string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout, Control: Control}
	return d.Dial(network, addr)
}

// DialTCP is DialTimeout restricted to TCP with a local address.
func DialTCP(laddr *net.TCPAddr, raddr *net.TCPAddr, timeout time.Duration) (*net.TCPConn, error) {
	d := net.Dialer{Timeout: timeout, Control: Control}
	if laddr != nil {
		d.LocalAddr = laddr
	}
	conn, err := d.Dial("tcp", raddr.String())
	if err != nil {
		return nil, err
	}
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("dial %s returned %T, want *net.TCPConn", raddr, conn)
	}
	return tcpConn, nil
}

// DialUDP dials a UDP socket with the bind control applied.
func DialUDP(laddr, raddr *net.UDPAddr) (*net.UDPConn, error) {
	d := net.Dialer{Control: Control}
	if laddr != nil {
		d.LocalAddr = laddr
	}
	conn, err := d.Dial("udp", raddr.String())
	if err != nil {
		return nil, err
	}
	udpConn, ok := conn.(*net.UDPConn)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("dial %s returned %T, want *net.UDPConn", raddr, conn)
	}
	return udpConn, nil
}

// BindUDPConn applies the interface pin to an already-created (typically
// unconnected) UDP socket, whose sends would otherwise follow the routing
// table back into the TUN adapter.
func BindUDPConn(c *net.UDPConn) error {
	if BindInterface() == 0 {
		return nil
	}
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var bindErr error
	err = rc.Control(func(fd uintptr) {
		bindErr = bindSocketFD(fd)
	})
	if err != nil {
		return err
	}
	return bindErr
}

// Dialer returns a net.Dialer preconfigured with the bind control, for
// libraries that construct their own sockets (DoH HTTP transport, miekg/dns
// clients).
func Dialer(timeout time.Duration) net.Dialer {
	return net.Dialer{Timeout: timeout, Control: Control}
}
