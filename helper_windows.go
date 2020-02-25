package main

import (
	"net"
)

// just test
func getOriginalDst(clientConn *net.TCPConn) (host string, port int, newTCPConn *net.TCPConn, err error) {
	host = "39.156.69.79"
	port = 443
	newTCPConn = clientConn
	err = nil
	return
}
