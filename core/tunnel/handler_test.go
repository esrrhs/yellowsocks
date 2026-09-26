package tunnel

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/esrrhs/gohome/network"
	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	appdns "github.com/esrrhs/yellowsocks/core/dns"
	"github.com/esrrhs/yellowsocks/core/router"
)

type mockSocks5 struct{ addr string }

func (m *mockSocks5) Socks5Addr() string { return m.addr }

type pipeUDPConn struct {
	net.Conn
	local, remote net.Addr
}

func (c *pipeUDPConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := c.Conn.Read(p)
	return n, c.remote, err
}
func (c *pipeUDPConn) WriteTo(p []byte, _ net.Addr) (int, error) { return c.Conn.Write(p) }
func (c *pipeUDPConn) LocalAddr() net.Addr                       { return c.local }
func (c *pipeUDPConn) RemoteAddr() net.Addr                      { return c.remote }
func (c *pipeUDPConn) ID() stack.TransportEndpointID             { return stack.TransportEndpointID{} }

type pipeTCPConn struct {
	net.Conn
	local, remote net.Addr
}

func (c *pipeTCPConn) LocalAddr() net.Addr           { return c.local }
func (c *pipeTCPConn) RemoteAddr() net.Addr          { return c.remote }
func (c *pipeTCPConn) ID() stack.TransportEndpointID { return stack.TransportEndpointID{} }

func TestSetProtectSocketInvokesCallback(t *testing.T) {
	h := NewHandler(nil, nil, nil)
	called := false
	h.SetProtectSocket(func(fd int) bool {
		called = true
		return true
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	d := &net.Dialer{Timeout: time.Second, Control: h.controlProtect}
	c, err := d.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial with protect: %v", err)
	}
	_ = c.Close()
	if !called {
		t.Fatal("expected protect callback to be invoked")
	}
}

func TestSetProtectSocketRejects(t *testing.T) {
	h := NewHandler(nil, nil, nil)
	h.SetProtectSocket(func(fd int) bool { return false })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	d := &net.Dialer{Timeout: time.Second, Control: h.controlProtect}
	if _, err := d.Dial("tcp", ln.Addr().String()); err == nil {
		t.Fatal("expected dial to fail when protect returns false")
	}
}

func TestSetProtectSocketNilClears(t *testing.T) {
	h := NewHandler(nil, nil, &mockSocks5{addr: "127.0.0.1:1"})
	h.SetProtectSocket(func(fd int) bool { return true })
	h.SetProtectSocket(nil)
	if h.protect != nil {
		t.Fatal("expected nil protect after clear")
	}
}

func TestHandleUDPDirectEcho(t *testing.T) {
	echo, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteToUDP(buf[:n], addr)
		}
	}()

	client, server := net.Pipe()
	defer client.Close()

	h := NewHandler(router.NewRouter(), nil, nil)
	tun := &pipeUDPConn{
		Conn:   server,
		local:  &net.UDPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 40000},
		remote: echo.LocalAddr(),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HandleUDP(tun)
	}()

	payload := []byte("udp-direct-echo")
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf[:n]) != string(payload) {
		t.Fatalf("got %q want %q", buf[:n], payload)
	}
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("HandleUDP did not exit")
	}
}

func TestHandleTCPDirectEcho(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()

	client, server := net.Pipe()
	defer client.Close()

	h := NewHandler(router.NewRouter(), nil, nil)
	tun := &pipeTCPConn{
		Conn:   server,
		local:  &net.TCPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 50000},
		remote: ln.Addr(),
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.HandleTCP(tun)
	}()

	payload := []byte("tcp-direct-echo")
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != string(payload) {
		t.Fatalf("got %q want %q", buf[:n], payload)
	}
	_ = client.Close()
	wg.Wait()
}

func TestHandleUDPFakeIPViaSPP(t *testing.T) {
	echo, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteToUDP(buf[:n], addr)
		}
	}()
	echoHost, echoPortStr, _ := net.SplitHostPort(echo.LocalAddr().String())
	echoPort, _ := strconv.Atoi(echoPortStr)

	tcpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpL.Close()

	udpRelay, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer udpRelay.Close()

	go serveMockSocks5UDP(t, tcpL, udpRelay)

	// DNS Fake-IP: allocate domain == echo host IP so SOCKS5 target resolves locally.
	dnsUDP := freeUDPAddr(t)
	dnsSrv, err := appdns.NewServer(appdns.Config{ListenAddr: dnsUDP, EnableFakeIP: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := dnsSrv.Start(); err != nil {
		t.Fatal(err)
	}
	defer dnsSrv.Stop()
	time.Sleep(50 * time.Millisecond)

	var fakeIP net.IP
	var lastErr error
	for i := 0; i < 20; i++ {
		fakeIP, lastErr = tryQueryFakeIP(dnsUDP, echoHost)
		if lastErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("dns exchange: %v", lastErr)
	}
	if !appdns.IsFakeIP(fakeIP) {
		t.Fatalf("expected fake-ip, got %s", fakeIP)
	}
	domain, ok := dnsSrv.LookupDomainByIP(fakeIP.String())
	if !ok {
		t.Fatalf("LookupDomainByIP failed for %s", fakeIP)
	}
	if domain != echoHost {
		t.Fatalf("LookupDomainByIP=%q want %q", domain, echoHost)
	}

	client, server := net.Pipe()
	defer client.Close()

	h := NewHandler(router.NewRouter(), dnsSrv, &mockSocks5{addr: tcpL.Addr().String()})
	tun := &pipeUDPConn{
		Conn:   server,
		local:  &net.UDPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 40001},
		remote: &net.UDPAddr{IP: fakeIP, Port: echoPort},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HandleUDP(tun)
	}()

	payload := []byte("udp-fakeip-spp")
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("read via fake-ip spp path: %v", err)
	}
	if string(buf[:n]) != string(payload) {
		t.Fatalf("got %q want %q", buf[:n], payload)
	}
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("HandleUDP did not exit")
	}
}

func TestFakeIPInvariant(t *testing.T) {
	pool := appdns.NewFakeIPPool()
	ip := pool.Allocate("quic.example.com")
	if !appdns.IsFakeIP(ip) {
		t.Fatalf("expected fake ip, got %s", ip)
	}
}

func serveMockSocks5UDP(t *testing.T, tcpL net.Listener, udpRelay *net.UDPConn) {
	t.Helper()
	go func() {
		for {
			c, err := tcpL.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = network.Sock5HandshakeBy(c, "", "")
				var head [4]byte
				if _, err := io.ReadFull(c, head[:]); err != nil {
					return
				}
				switch head[3] {
				case 0x01:
					_, _ = io.CopyN(io.Discard, c, 6)
				case 0x03:
					var l [1]byte
					_, _ = io.ReadFull(c, l[:])
					_, _ = io.CopyN(io.Discard, c, int64(l[0])+2)
				case 0x04:
					_, _ = io.CopyN(io.Discard, c, 18)
				}
				relay := udpRelay.LocalAddr().(*net.UDPAddr)
				ip := relay.IP.To4()
				reply := []byte{0x05, 0x00, 0x00, 0x01, ip[0], ip[1], ip[2], ip[3],
					byte(relay.Port >> 8), byte(relay.Port)}
				_, _ = c.Write(reply)
				_, _ = io.Copy(io.Discard, c)
			}(c)
		}
	}()
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := udpRelay.ReadFromUDP(buf)
			if err != nil {
				return
			}
			dstH, dstP, payload, err := network.Sock5UnpackUDP(buf[:n])
			if err != nil {
				continue
			}
			dstAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(dstH, strconv.Itoa(dstP)))
			if err != nil {
				continue
			}
			tmp, err := net.DialUDP("udp", nil, dstAddr)
			if err != nil {
				continue
			}
			_, _ = tmp.Write(payload)
			_ = tmp.SetReadDeadline(time.Now().Add(2 * time.Second))
			resp := make([]byte, 65535)
			rn, rerr := tmp.Read(resp)
			_ = tmp.Close()
			if rerr != nil || rn == 0 {
				continue
			}
			packed, _ := network.Sock5PackUDP(from.IP.String(), from.Port, resp[:rn])
			_, _ = udpRelay.WriteToUDP(packed, from)
		}
	}()
}

func freeUDPAddr(t *testing.T) string {
	t.Helper()
	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.LocalAddr().String()
	_ = l.Close()
	return addr
}

func queryFakeIP(t *testing.T, dnsAddr, name string) net.IP {
	t.Helper()
	ip, err := tryQueryFakeIP(dnsAddr, name)
	if err != nil {
		t.Fatalf("dns exchange: %v", err)
	}
	return ip
}

func tryQueryFakeIP(dnsAddr, name string) (net.IP, error) {
	c := new(dns.Client)
	c.Timeout = 2 * time.Second
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	resp, _, err := c.Exchange(m, dnsAddr)
	if err != nil {
		return nil, err
	}
	for _, rr := range resp.Answer {
		if a, ok := rr.(*dns.A); ok {
			return a.A, nil
		}
	}
	return nil, fmt.Errorf("no A answer for %s", name)
}
