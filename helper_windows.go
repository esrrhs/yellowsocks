package main

import (
	"net"
)

// just test
func getOriginalDst(clientConn *net.TCPConn) (host string, port int, err error) {
	host = "183.232.231.174"
	port = 443
	err = nil
	return
}
