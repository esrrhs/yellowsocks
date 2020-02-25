package main

import (
	"fmt"
	"net"
	"syscall"
)

const (
	SO_ORIGINAL_DST = 80
)

func getOriginalDst(clientConn *net.TCPConn) (host string, port int, err error) {

	file, err := clientConn.File()
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	fd := file.Fd()

	addr, err :=
		syscall.GetsockoptIPv6Mreq(
			int(fd),
			syscall.IPPROTO_IP,
			SO_ORIGINAL_DST)
	if err != nil {
		return "", 0, err
	}

	rawaddr := make([]byte, 0)

	// \attention: IPv4 only!!!
	// address type, 1 - IPv4, 4 - IPv6, 3 - hostname, only IPv4 is supported now
	rawaddr = append(rawaddr, byte(1))
	// raw IP address, 4 bytes for IPv4 or 16 bytes for IPv6, only IPv4 is supported now
	rawaddr = append(rawaddr, addr.Multiaddr[4])
	rawaddr = append(rawaddr, addr.Multiaddr[5])
	rawaddr = append(rawaddr, addr.Multiaddr[6])
	rawaddr = append(rawaddr, addr.Multiaddr[7])
	// port
	rawaddr = append(rawaddr, addr.Multiaddr[2])
	rawaddr = append(rawaddr, addr.Multiaddr[3])

	host = fmt.Sprintf("%d.%d.%d.%d",
		addr.Multiaddr[4],
		addr.Multiaddr[5],
		addr.Multiaddr[6],
		addr.Multiaddr[7])

	port = int(uint16(addr.Multiaddr[2])<<8 + uint16(addr.Multiaddr[3]))

	return
}
