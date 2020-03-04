package main

import (
	"flag"
	"github.com/esrrhs/go-engine/src/common"
	"github.com/esrrhs/go-engine/src/loggo"
	"github.com/esrrhs/go-engine/src/network"
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
	loggo.Info("listen ok %s", tcpaddr.String())

	dstaddr, err := net.ResolveTCPAddr("tcp", *target)
	if err != nil {
		loggo.Error("target fail %s", err)
		return
	}
	loggo.Info("target %s", dstaddr.String())

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

	defer common.CrashLog()

	loggo.Info("start conn from %s", conn.RemoteAddr())

	host, port, err := getOriginalDst(conn)

	loggo.Info("parse conn from %s -> %s:%d", conn.RemoteAddr(), host, port)

	socks5conn, err := net.DialTCP("tcp", nil, socks5addr)
	if err != nil {
		loggo.Info("dial socks5 conn fail %s %v", socks5addr, err)
		return
	}

	loggo.Info("dial socks5 conn ok %s -> %s:%d", conn.RemoteAddr(), host, port)

	err = network.Sock5Handshake(socks5conn)
	if err != nil {
		loggo.Error("sock5Handshake fail %s", err)
		return
	}

	loggo.Info("Handshake socks5 conn ok %s -> %s:%d", conn.RemoteAddr(), host, port)

	err = network.Sock5SetRequest(socks5conn, host, port)
	if err != nil {
		conn.Close()
		loggo.Error("sock5SetRequest fail %s", err)
		return
	}

	loggo.Info("SetRequest socks5 conn ok %s -> %s:%d", conn.RemoteAddr(), host, port)

	go transfer(conn, socks5conn, conn.RemoteAddr().String(), socks5conn.RemoteAddr().String())
	go transfer(socks5conn, conn, socks5conn.RemoteAddr().String(), conn.RemoteAddr().String())

	loggo.Info("process conn ok %s -> %s:%d", conn.RemoteAddr(), host, port)
}

func transfer(destination io.WriteCloser, source io.ReadCloser, dst string, src string) {
	defer common.CrashLog()
	defer destination.Close()
	defer source.Close()
	loggo.Info("begin transfer from %s -> %s", src, dst)
	io.Copy(destination, source)
	loggo.Info("end transfer from %s -> %s", src, dst)
}
