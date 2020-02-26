package main

import (
	"net"
)

// just test
func getOriginalDst(clientConn *net.TCPConn) (host string, port int, err error) {
	host = "39.106.101.133"
	port = 22
	err = nil
	return
}
