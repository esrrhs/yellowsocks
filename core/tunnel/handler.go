package tunnel

import (
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/gohome/network"
	"github.com/xjasonlyu/tun2socks/v2/core/adapter"
	appdns "github.com/esrrhs/yellowsocks/core/dns"
	"github.com/esrrhs/yellowsocks/core/router"
	"github.com/esrrhs/yellowsocks/core/stats"
)

// Socks5Provider 提供本地 Socks5 端口
type Socks5Provider interface {
	Socks5Addr() string
}

// Handler 实现 tun2socks v2 的 TransportHandler 接口
type Handler struct {
	router    *router.Router
	dnsServer *appdns.Server
	sppClient Socks5Provider
}

func NewHandler(r *router.Router, dnsSrv *appdns.Server, spp Socks5Provider) *Handler {
	return &Handler{
		router:    r,
		dnsServer: dnsSrv,
		sppClient: spp,
	}
}

// HandleTCP 处理来自 TUN 虚拟网卡的 TCP 连接
func (h *Handler) HandleTCP(conn adapter.TCPConn) {
	defer common.CrashLog()
	defer conn.Close()

	rAddr := conn.RemoteAddr()
	if rAddr == nil {
		return
	}

	tcpAddr, err := net.ResolveTCPAddr(rAddr.Network(), rAddr.String())
	if err != nil {
		return
	}

	destIP := tcpAddr.IP
	destHost := ""
	if h.dnsServer != nil {
		if host, ok := h.dnsServer.LookupDomainByIP(destIP.String()); ok {
			destHost = host
		}
	}

	srcPort := 0
	if lAddr := conn.LocalAddr(); lAddr != nil {
		if lTCP, err := net.ResolveTCPAddr(lAddr.Network(), lAddr.String()); err == nil {
			srcPort = lTCP.Port
		}
	}

	decision := router.Proxy
	if h.router != nil {
		decision = h.router.DecideWithPort(destHost, destIP, srcPort)
	}

	ruleStr := "Proxy (SPP)"
	if decision == router.Direct {
		ruleStr = "Direct"
	}

	procName := "unknown"
	if h.router != nil && h.router.Inspector() != nil {
		if p, ok := h.router.Inspector().GetProcessByPort(srcPort); ok {
			procName = p
		}
	}

	connID := conn.LocalAddr().String() + "->" + tcpAddr.String()
	stats.Default.TrackConnection(connID, procName, conn.LocalAddr().String(), tcpAddr.String(), destHost, ruleStr)
	defer stats.Default.RemoveConnection(connID)

	loggo.Info("[Tunnel] TCP %s (%s) -> %s (Domain: %s, Decision: %s)",
		conn.LocalAddr(), procName, tcpAddr.String(), destHost, ruleStr)

	stats.Default.AddLog(procName + " -> " + tcpAddr.String() + " [" + ruleStr + "]")

	if decision == router.Direct {
		h.forwardDirectTCP(conn, tcpAddr, connID)
	} else {
		h.forwardSppTCP(conn, destHost, tcpAddr, connID)
	}
}

// HandleUDP 处理来自 TUN 虚拟网卡的 UDP 会话/数据报
func (h *Handler) HandleUDP(conn adapter.UDPConn) {
	defer common.CrashLog()
	defer conn.Close()

	rAddr := conn.RemoteAddr()
	if rAddr == nil {
		return
	}

	udpAddr, err := net.ResolveUDPAddr(rAddr.Network(), rAddr.String())
	if err != nil {
		return
	}

	// 拦截 UDP 53 端口的 DNS 请求
	if udpAddr.Port == 53 {
		h.handleDNSUDP(conn, udpAddr)
		return
	}

	// 其他境外 UDP 如果是境内则直连
	decision := router.Proxy
	if h.router != nil {
		decision = h.router.Decide("", udpAddr.IP)
	}

	if decision == router.Direct {
		uConn, err := net.DialUDP("udp", nil, udpAddr)
		if err != nil {
			return
		}
		defer uConn.Close()

		go func() {
			buf := make([]byte, 2048)
			for {
				n, err := uConn.Read(buf)
				if err != nil {
					break
				}
				_, _ = conn.Write(buf[:n])
			}
		}()

		buf := make([]byte, 2048)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				break
			}
			_, _ = uConn.Write(buf[:n])
		}
	} else {
		h.forwardSppUDP(conn, udpAddr)
	}
}

func (h *Handler) forwardSppUDP(conn adapter.UDPConn, target *net.UDPAddr) {
	if h.sppClient == nil || h.sppClient.Socks5Addr() == "" {
		return
	}

	sppTCPAddr, err := net.ResolveTCPAddr("tcp", h.sppClient.Socks5Addr())
	if err != nil {
		return
	}
	sppTCPConn, err := net.DialTCP("tcp", nil, sppTCPAddr)
	if err != nil {
		return
	}
	defer sppTCPConn.Close()

	if err := network.Sock5Handshake(sppTCPConn, 5000, "", ""); err != nil {
		return
	}

	bnd, err := network.Sock5SetUDPRequest(sppTCPConn, "0.0.0.0", 0, 5000)
	if err != nil {
		return
	}

	bndH, bndP, err := net.SplitHostPort(bnd)
	if err != nil {
		return
	}
	p, _ := strconv.Atoi(bndP)
	bndIP := net.ParseIP(bndH)
	if bndIP == nil || bndIP.IsUnspecified() {
		bndIP = sppTCPConn.RemoteAddr().(*net.TCPAddr).IP
	}
	relayTarget := &net.UDPAddr{IP: bndIP, Port: p}

	uConn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return
	}
	defer uConn.Close()

	done := make(chan struct{})

	go func() {
		defer close(done)
		buf := make([]byte, 65535)
		for {
			n, _, err := uConn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _, payload, err := network.Sock5UnpackUDP(buf[:n])
			if err == nil && len(payload) > 0 {
				_, _ = conn.Write(payload)
			}
		}
	}()

	go func() {
		buf := make([]byte, 65535)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				_ = uConn.Close()
				return
			}
			packed, err := network.Sock5PackUDP(target.IP.String(), target.Port, buf[:n])
			if err == nil {
				_, _ = uConn.WriteToUDP(packed, relayTarget)
			}
		}
	}()

	<-done
}

func (h *Handler) handleDNSUDP(conn adapter.UDPConn, target *net.UDPAddr) {
	localDNSAddr := "127.0.0.1:53"
	rAddr, err := net.ResolveUDPAddr("udp", localDNSAddr)
	if err != nil {
		return
	}

	uConn, err := net.DialUDP("udp", nil, rAddr)
	if err != nil {
		return
	}
	defer uConn.Close()

	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		return
	}

	_, err = uConn.Write(buf[:n])
	if err != nil {
		return
	}

	_ = uConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	respBuf := make([]byte, 4096)
	rn, err := uConn.Read(respBuf)
	if err == nil && rn > 0 {
		_, _ = conn.Write(respBuf[:rn])
	}
}

func (h *Handler) forwardDirectTCP(conn net.Conn, target *net.TCPAddr, connID string) {
	outConn, err := net.DialTimeout("tcp", target.String(), 5*time.Second)
	if err != nil {
		loggo.Error("[Tunnel] Direct dial %s failed: %v", target.String(), err)
		return
	}
	defer outConn.Close()

	relay(conn, outConn, connID)
}

func (h *Handler) forwardSppTCP(conn net.Conn, host string, target *net.TCPAddr, connID string) {
	if h.sppClient == nil {
		loggo.Error("[Tunnel] SPP client is nil, cannot forward")
		return
	}

	socks5Addr := h.sppClient.Socks5Addr()
	tcpAddr, err := net.ResolveTCPAddr("tcp", socks5Addr)
	if err != nil {
		loggo.Error("[Tunnel] Resolve SPP socks5 %s failed: %v", socks5Addr, err)
		return
	}

	s5Conn, err := net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		loggo.Error("[Tunnel] Dial SPP socks5 %s failed: %v", socks5Addr, err)
		return
	}
	defer s5Conn.Close()

	// Socks5 握手
	err = network.Sock5Handshake(s5Conn, 0, "", "")
	if err != nil {
		loggo.Error("[Tunnel] Socks5 handshake failed: %v", err)
		return
	}

	targetHost := target.IP.String()
	if host != "" {
		targetHost = host
	}

	err = network.Sock5SetRequest(s5Conn, targetHost, target.Port, 0)
	if err != nil {
		loggo.Error("[Tunnel] Socks5 set request %s:%d failed: %v", targetHost, target.Port, err)
		return
	}

	relay(conn, s5Conn, connID)
}

type countingWriter struct {
	w      io.Writer
	connID string
	isUp   bool
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	if n > 0 {
		if cw.isUp {
			stats.Default.UpdateConnectionTraffic(cw.connID, int64(n), 0)
		} else {
			stats.Default.UpdateConnectionTraffic(cw.connID, 0, int64(n))
		}
	}
	return n, err
}

func relay(left, right net.Conn, connID string) {
	var wg sync.WaitGroup
	wg.Add(2)

	// left -> right (上传)
	go func() {
		defer wg.Done()
		cw := &countingWriter{w: right, connID: connID, isUp: true}
		_, _ = io.Copy(cw, left)
		if tcpConn, ok := right.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
	}()

	// right -> left (下载)
	go func() {
		defer wg.Done()
		cw := &countingWriter{w: left, connID: connID, isUp: false}
		_, _ = io.Copy(cw, right)
		if tcpConn, ok := left.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		}
	}()

	wg.Wait()
}
