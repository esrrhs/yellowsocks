package proxy

import (
	"net"
	"strconv"
	"strings"
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
	ipv6 := remote.To4() == nil && remote != nil
	if remote == nil && local.To4() == nil && local != nil {
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
		if local.To4() == nil && local != nil && !local.IsUnspecified() {
			replyIP = local.String()
		}
	} else if v4 := local.To4(); v4 != nil && !v4.IsUnspecified() {
		replyIP = v4.String()
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
