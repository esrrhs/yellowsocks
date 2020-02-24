package main

import (
	"flag"
	"github.com/esrrhs/go-engine/src/common"
	"github.com/esrrhs/go-engine/src/loggo"
	"io"
	"net"
)

func main() {

	defer common.CrashLog()

	listen := flag.String("l", "", "listen addr")
	target := flag.String("t", "", "target addr")
	nolog := flag.Int("nolog", 0, "write log file")
	noprint := flag.Int("noprint", 0, "print stdout")
	loglevel := flag.String("loglevel", "info", "log level")

	flag.Parse()

	if *listen == "" || *target == "" {
		flag.Usage()
		return
	}

	level := loggo.LEVEL_INFO
	if loggo.NameToLevel(*loglevel) >= 0 {
		level = loggo.NameToLevel(*loglevel)
	}
	loggo.Ini(loggo.Config{
		Level:     level,
		Prefix:    "yellowsocks",
		MaxDay:    3,
		NoLogFile: *nolog > 0,
		NoPrint:   *noprint > 0,
	})
	loggo.Info("start...")

	tcpaddr, err := net.ResolveTCPAddr("tcp", *listen)
	if err != nil {
		loggo.Error("listen fail %s", err)
		return
	}

	tcplistenConn, err := net.ListenTCP("tcp", tcpaddr)
	if err != nil {
		loggo.Error("Error listening for tcp packets: %s", err)
		return
	}

	dstaddr, err := net.ResolveTCPAddr("tcp", *target)
	if err != nil {
		loggo.Error("target fail %s", err)
		return
	}

	for {
		conn, err := tcplistenConn.AcceptTCP()
		if err != nil {
			loggo.Info("Error accept tcp %s", err)
			continue
		}

		go process(conn, dstaddr)
	}
}

func process(conn *net.TCPConn, socks5addr *net.TCPAddr) {

	loggo.Info("start conn from %s", conn.RemoteAddr())

	host, port := getConnOldDst(conn)

	loggo.Info("parse conn from %s -> %s:%d", conn.RemoteAddr(), host, port)

	socks5conn, err := net.DialTCP("tcp", nil, socks5addr)
	if err != nil {
		return
	}

	err = sock5Handshake(socks5conn)
	if err != nil {
		loggo.Error("sock5Handshake fail %s", err)
		return
	}

	err = sock5SetRequest(socks5conn, host, port)
	if err != nil {
		conn.Close()
		loggo.Error("sock5SetRequest fail %s", err)
		return
	}

	go transfer(conn, socks5conn)
	go transfer(socks5conn, conn)
}

func transfer(destination io.WriteCloser, source io.ReadCloser) {
	defer destination.Close()
	defer source.Close()
	io.Copy(destination, source)
}

func getConnOldDst(conn *net.TCPConn) (string, int) {
	return "", 0
}
