package proxy

import (
	"io"
	"net"
	"strconv"
	"sync"
	"testing"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/gohome/network"
	"github.com/esrrhs/yellowsocks/core/router"
	"github.com/esrrhs/yellowsocks/core/stats"
)

// Benchmarks silence the INFO logs: they interleave with the results and
// would otherwise dominate a per-connection benchmark. Their cost is
// measured separately by BenchmarkLoggoInfo.
var benchQuietOnce sync.Once

func benchQuiet() {
	benchQuietOnce.Do(func() {
		loggo.Ini(loggo.Config{Level: loggo.LEVEL_ERROR, Prefix: "bench", NoLogFile: true})
	})
}

// benchEchoTCP accepts connections and echoes every byte back. It stands in
// for the origin server at the far end of the relay.
func benchEchoTCP(b testing.TB) (string, func()) {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("echo listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// RST instead of FIN: without it a churn benchmark walks
				// into ephemeral port exhaustion on TIME_WAIT sockets.
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.SetLinger(0)
				}
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

// tcpPair returns two ends of a loopback TCP connection.
func tcpPair(b testing.TB) (net.Conn, net.Conn) {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("pair listen: %v", err)
	}
	defer ln.Close()
	type result struct {
		c   net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		ch <- result{c, err}
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		b.Fatalf("pair dial: %v", err)
	}
	r := <-ch
	if r.err != nil {
		b.Fatalf("pair accept: %v", r.err)
	}
	return client, r.c
}

// BenchmarkRelayStreams measures the copy loop itself: how fast bytes move
// between two sockets.
func BenchmarkRelayStreams(b *testing.B) {
	benchQuiet()

	const payload = 1 << 20
	echoAddr, stop := benchEchoTCP(b)
	defer stop()

	buf := make([]byte, payload)
	sink := make([]byte, payload)

	b.SetBytes(payload)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		clientLocal, clientRemote := tcpPair(b)
		target, err := net.Dial("tcp", echoAddr)
		if err != nil {
			b.Fatalf("dial echo: %v", err)
		}

		done := make(chan struct{})
		go func() {
			relayStreams(clientLocal, target, defaultIdleTimeout, nil)
			close(done)
		}()

		if _, err := clientRemote.Write(buf); err != nil {
			b.Fatalf("write: %v", err)
		}
		if _, err := io.ReadFull(clientRemote, sink); err != nil {
			b.Fatalf("read: %v", err)
		}
		_ = clientRemote.Close()
		<-done
		_ = target.Close()
		_ = clientLocal.Close()
	}
}

// startDirectSocks5 runs a SOCKS5 server whose router sends everything
// direct, so a benchmark exercises accept, handshake, routing, dial and
// relay without needing an SPP upstream.
func startDirectSocks5(b testing.TB) (string, func()) {
	b.Helper()
	r := router.NewRouterWithOptions(router.Options{})
	r.AddDirectDomain("127.0.0.1")
	srv := NewSocks5Server(Socks5Config{
		ListenAddr: "127.0.0.1:0",
		Router:     r,
		Upstream:   &mockUpstream{addr: "127.0.0.1:1"},
	})
	if err := srv.Start(); err != nil {
		b.Fatalf("socks5 start: %v", err)
	}
	return srv.Addr().String(), func() { _ = srv.Stop() }
}

// startDirectSocks5WithStats is startDirectSocks5 with a live statistics
// collector attached, so benchmarks exercise the accounting done on every
// relayed chunk.
func startDirectSocks5WithStats(b testing.TB, c *stats.Collector) (string, func()) {
	b.Helper()
	r := router.NewRouterWithOptions(router.Options{})
	r.AddDirectDomain("127.0.0.1")
	srv := NewSocks5Server(Socks5Config{
		ListenAddr: "127.0.0.1:0",
		Router:     r,
		Upstream:   &mockUpstream{addr: "127.0.0.1:1"},
		Stats:      c,
	})
	if err := srv.Start(); err != nil {
		b.Fatalf("socks5 start: %v", err)
	}
	return srv.Addr().String(), func() { _ = srv.Stop() }
}

// socks5RoundTrip opens a connection through the proxy to target, writes
// payload bytes and reads them back from the echo server.
func socks5RoundTrip(b testing.TB, proxyAddr, target string, payload int, buf, sink []byte) {
	b.Helper()
	tcpAddr, err := net.ResolveTCPAddr("tcp", proxyAddr)
	if err != nil {
		b.Fatalf("resolve proxy: %v", err)
	}
	conn, err := net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		b.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	_ = conn.SetLinger(0)
	if err := network.Sock5Handshake(conn, 5000, "", ""); err != nil {
		b.Fatalf("handshake: %v", err)
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		b.Fatalf("split target: %v", err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		b.Fatalf("port: %v", err)
	}
	if err := network.Sock5SetRequest(conn, host, p, 5000); err != nil {
		b.Fatalf("request: %v", err)
	}
	if _, err := conn.Write(buf[:payload]); err != nil {
		b.Fatalf("write: %v", err)
	}
	if _, err := io.ReadFull(conn, sink[:payload]); err != nil {
		b.Fatalf("read: %v", err)
	}
}

// BenchmarkSocks5Throughput is the bulk-transfer case: one connection, a
// lot of bytes. It shows the cost of the copy loop and the syscalls around
// it.
func BenchmarkSocks5Throughput(b *testing.B) {
	benchQuiet()

	const payload = 1 << 20
	echoAddr, stopEcho := benchEchoTCP(b)
	defer stopEcho()
	proxyAddr, stopProxy := startDirectSocks5(b)
	defer stopProxy()

	buf := make([]byte, payload)
	sink := make([]byte, payload)

	b.SetBytes(payload)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		socks5RoundTrip(b, proxyAddr, echoAddr, payload, buf, sink)
	}
}

// BenchmarkSocks5ThroughputWithStats is BenchmarkSocks5Throughput with the
// statistics collector attached: it measures what accounting costs on a
// bulk transfer.
func BenchmarkSocks5ThroughputWithStats(b *testing.B) {
	benchQuiet()

	const payload = 1 << 20
	echoAddr, stopEcho := benchEchoTCP(b)
	defer stopEcho()
	c := stats.New(stats.Config{HistoryMinutes: 60})
	defer c.Close()
	proxyAddr, stopProxy := startDirectSocks5WithStats(b, c)
	defer stopProxy()

	buf := make([]byte, payload)
	sink := make([]byte, payload)

	b.SetBytes(payload)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		socks5RoundTrip(b, proxyAddr, echoAddr, payload, buf, sink)
	}
}

// BenchmarkSocks5Concurrent is the realistic case: many connections in
// flight at once, statistics on. One saturated connection is syscall bound
// and hides the shared-state costs; this one exposes them.
func BenchmarkSocks5Concurrent(b *testing.B) {
	benchQuiet()

	const payload = 64 << 10
	echoAddr, stopEcho := benchEchoTCP(b)
	defer stopEcho()
	c := stats.New(stats.Config{HistoryMinutes: 60})
	defer c.Close()
	proxyAddr, stopProxy := startDirectSocks5WithStats(b, c)
	defer stopProxy()

	b.SetBytes(payload)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		buf := make([]byte, payload)
		sink := make([]byte, payload)
		for pb.Next() {
			socks5RoundTrip(b, proxyAddr, echoAddr, payload, buf, sink)
		}
	})
}

// BenchmarkLoggoInfo measures what one INFO line costs. Every proxied
// connection logs one, so this is the per-connection tax at the default log
// level.
//
// Run it on its own (-bench LoggoInfo): loggo.Ini only takes effect once per
// process, so this benchmark has to be the one that initialises logging.
func BenchmarkLoggoInfo(b *testing.B) {
	loggo.Ini(loggo.Config{Level: loggo.LEVEL_INFO, Prefix: "bench", NoLogFile: true})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		loggo.Info("[SOCKS5] TCP CONNECT %s -> %s (%s, Decision: %s)",
			"127.0.0.1:54321", "93.184.216.34:443", "example.com", "Proxy (SPP)")
	}
}

// BenchmarkSocks5Connections is the churn case: many short connections. It
// shows the per-connection cost of handshake, routing and setup.
func BenchmarkSocks5Connections(b *testing.B) {
	benchQuiet()

	const payload = 4 << 10
	echoAddr, stopEcho := benchEchoTCP(b)
	defer stopEcho()
	proxyAddr, stopProxy := startDirectSocks5(b)
	defer stopProxy()

	buf := make([]byte, payload)
	sink := make([]byte, payload)

	b.SetBytes(payload)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		socks5RoundTrip(b, proxyAddr, echoAddr, payload, buf, sink)
	}
}
