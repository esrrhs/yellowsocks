package proxy

import "net"

// ProcessResolver attributes a relayed connection to the OS process that
// opened it. Implementations are platform-specific (Windows reads the
// connection tables; the CLI ships without one), and both return values may
// be empty when the owner cannot be determined.
//
// The lookup strategy differs by who the client is:
//   - TUN-captured traffic: the client is the capture engine, and the
//     destination (dstHost, a real or fake address, plus dstPort) is what
//     the original application actually dialed — the connection table holds
//     a row whose remote address matches it, owned by the app.
//   - Loopback proxy clients (system proxy): the client socket itself
//     belongs to the application, so the row is found by its local port.
type ProcessResolver interface {
	FindProcess(srcIP net.IP, srcPort int, dstHost string, dstPort int) (name string, icon string)
}
