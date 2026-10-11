package proxy

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
)

// canonicalHost rewrites an address the way gohome's SOCKS encoder expects it.
// IPv4-mapped IPv6 becomes IPv4. A real IPv6 address is left as IPv6 text.
func canonicalHost(host string) string {
	host = strings.TrimSpace(host)
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
		return ip.String()
	}
	return host
}

// isIPv6Literal returns true if host is an IPv6 address (excluding IPv4-mapped IPv6).
func isIPv6Literal(host string) bool {
	host = strings.TrimSpace(host)
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.To4() == nil
}

func joinDialAddr(host string, port int) string {
	return net.JoinHostPort(canonicalHost(host), strconv.Itoa(port))
}

func hostFromAddr(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// listenUDPFor opens a socket that can send to host. IPv6 literals use udp6
// so the write is not stuck on an IPv4-only wildcard socket.
func listenUDPFor(host string) (*net.UDPConn, error) {
	if ip := net.ParseIP(canonicalHost(host)); ip != nil && ip.To4() == nil {
		return net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified, Port: 0})
	}
	return net.ListenUDP("udp", nil)
}

// listenClientUDPRelay binds the SOCKS5 UDP relay on the same family as the
// client TCP connection. An IPv6 client cannot send to an IPv4 wildcard socket.
func listenClientUDPRelay(client net.Conn) (*net.UDPConn, string, error) {
	local := addrIP(client.LocalAddr())
	remote := addrIP(client.RemoteAddr())
	// nil guards must precede To4(): addrIP returns nil for a non-IP
	// address, and calling To4() on nil would panic.
	ipv6 := remote != nil && remote.To4() == nil
	if remote == nil && local != nil && local.To4() == nil {
		ipv6 = true
	}

	network := "udp4"
	laddr := &net.UDPAddr{IP: net.IPv4zero, Port: 0}
	if ipv6 {
		network = "udp6"
		laddr = &net.UDPAddr{IP: net.IPv6unspecified, Port: 0}
	}
	conn, err := net.ListenUDP(network, laddr)
	if err != nil {
		return nil, "", err
	}

	port := conn.LocalAddr().(*net.UDPAddr).Port
	replyIP := "127.0.0.1"
	if ipv6 {
		replyIP = "::1"
		if local != nil && local.To4() == nil && !local.IsUnspecified() {
			replyIP = local.String()
		}
	} else if local != nil {
		if v4 := local.To4(); v4 != nil && !v4.IsUnspecified() {
			replyIP = v4.String()
		}
	}
	return conn, net.JoinHostPort(replyIP, strconv.Itoa(port)), nil
}

func addrIP(addr net.Addr) net.IP {
	if addr == nil {
		return nil
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return net.ParseIP(addr.String())
	}
	return net.ParseIP(host)
}

// sameUDPAddr reports whether two UDP endpoints share IP and port.
func sameUDPAddr(a, b *net.UDPAddr) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Port == b.Port && a.IP.Equal(b.IP)
}

// maxDirectUDPSessions bounds how many distinct direct targets a single UDP
// association may open sockets to. Each entry owns a socket and a read goroutine;
// without the limit one client could exhaust the process's file descriptors.
const maxDirectUDPSessions = 256

// directUDPRegistry tracks one outbound UDP socket per direct target.
type directUDPRegistry struct {
	mu    sync.Mutex
	conns map[string]*net.UDPConn
}

func newDirectUDPRegistry() *directUDPRegistry {
	return &directUDPRegistry{conns: make(map[string]*net.UDPConn)}
}

// get returns the cached socket for targetAddr, creating one on first use. fresh is
// true when a new socket was made so the caller can start its read loop.
func (r *directUDPRegistry) get(targetAddr string) (conn *net.UDPConn, fresh bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.conns[targetAddr]; ok {
		return c, false, nil
	}
	if len(r.conns) >= maxDirectUDPSessions {
		return nil, false, fmt.Errorf("too many direct UDP sessions (%d cap)", maxDirectUDPSessions)
	}
	c, err := listenUDPFor(hostFromAddr(targetAddr))
	if err != nil {
		return nil, false, err
	}
	r.conns[targetAddr] = c
	return c, true, nil
}

// closeAll releases every tracked socket; afterward the registry must not be used.
func (r *directUDPRegistry) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conns {
		_ = c.Close()
	}
	r.conns = map[string]*net.UDPConn{}
}
