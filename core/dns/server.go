package dns

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/miekg/dns"
	"golang.org/x/net/proxy"
	"github.com/esrrhs/yellowsocks/core/router"
)

// DNSCacheEntry 缓存单元
type DNSCacheEntry struct {
	Msg       *dns.Msg
	ExpiresAt time.Time
}

// Server DNS 拦截服务
type Server struct {
	listenAddr    string
	dohURL        string
	chinaDNS      string
	router        *router.Router
	socks5Addr    string // 用于境外 DoH 请求代理
	udpServer     *dns.Server
	cache         sync.Map // domain+type -> DNSCacheEntry
	ipToDomain    sync.Map // ip.String() -> domain (反查分流使用)
	fakeIPPool    *FakeIPPool
	enableFakeIP  bool
	httpClientDoH *http.Client
	mu            sync.RWMutex
}

// Config DNS 服务参数
type Config struct {
	ListenAddr   string // 监听地址，如 127.0.0.1:53 或 10.255.0.1:53
	DoHURL       string // 境外 DoH 解析地址，如 https://1.1.1.1/dns-query
	ChinaDNS     string // 境内常规 DNS 地址，如 223.5.5.5:53
	Socks5Addr   string // 境外代理 socks5 地址
	Router       *router.Router
	EnableFakeIP bool // 是否开启 Fake-IP 模式 (境外域名秒响应)
}

// NewServer 创建 DNS 拦截服务器
func NewServer(cfg Config) (*Server, error) {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:53"
	}
	if cfg.DoHURL == "" {
		cfg.DoHURL = "https://1.1.1.1/dns-query"
	}
	if cfg.ChinaDNS == "" {
		cfg.ChinaDNS = "223.5.5.5:53"
	}

	s := &Server{
		listenAddr:   cfg.ListenAddr,
		dohURL:       cfg.DoHURL,
		chinaDNS:     cfg.ChinaDNS,
		router:       cfg.Router,
		socks5Addr:   cfg.Socks5Addr,
		enableFakeIP: cfg.EnableFakeIP,
		fakeIPPool:   NewFakeIPPool(),
	}

	s.setupDoHClient()
	return s, nil
}

func (s *Server) setupDoHClient() {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: false},
	}

	// 如果配置了 Socks5，DoH 请求经由 Socks5 出境
	if s.socks5Addr != "" {
		dialer, err := proxy.SOCKS5("tcp", s.socks5Addr, nil, proxy.Direct)
		if err == nil {
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
		} else {
			loggo.Error("[DNS] Failed to create socks5 dialer for DoH: %v", err)
		}
	}

	s.httpClientDoH = &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}
}

// UpdateSocks5Addr 动态更新 socks5 代理地址
func (s *Server) UpdateSocks5Addr(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.socks5Addr = addr
	s.setupDoHClient()
}

// Start 启动 UDP DNS 监听
func (s *Server) Start() error {
	mux := dns.NewServeMux()
	mux.HandleFunc(".", s.handleDNSRequest)

	s.udpServer = &dns.Server{
		Addr:    s.listenAddr,
		Net:     "udp",
		Handler: mux,
	}

	loggo.Info("[DNS] Interceptor starting on UDP %s (China: %s, DoH: %s)", s.listenAddr, s.chinaDNS, s.dohURL)
	go func() {
		if err := s.udpServer.ListenAndServe(); err != nil {
			loggo.Error("[DNS] ListenAndServe failed: %v", err)
		}
	}()
	return nil
}

// Stop 停止 DNS 监听
func (s *Server) Stop() error {
	if s.udpServer != nil {
		return s.udpServer.Shutdown()
	}
	return nil
}

func (s *Server) handleDNSRequest(w dns.ResponseWriter, r *dns.Msg) {
	if len(r.Question) == 0 {
		return
	}

	q := r.Question[0]
	qName := strings.ToLower(strings.Trim(q.Name, "."))
	cacheKey := fmt.Sprintf("%s_%d_%d", qName, q.Qtype, q.Qclass)

	// 1. 查缓存
	if val, ok := s.cache.Load(cacheKey); ok {
		entry := val.(DNSCacheEntry)
		if time.Now().Before(entry.ExpiresAt) {
			resp := entry.Msg.Copy()
			resp.Id = r.Id
			_ = w.WriteMsg(resp)
			return
		}
		s.cache.Delete(cacheKey)
	}

	// 2. 根据分流策略选择解析方式
	var resp *dns.Msg
	var err error

	isDirect := s.router != nil && s.router.ShouldDirectDomain(qName)
	if isDirect {
		resp, err = s.resolveChinaDNS(r)
	} else if s.enableFakeIP && q.Qtype == dns.TypeA {
		// Fake-IP 模式：直接为境外域名秒级分发 198.18.x.x 虚假 IP
		fakeIP := s.fakeIPPool.Allocate(qName)
		resp = new(dns.Msg)
		resp.SetReply(r)
		rr := &dns.A{
			Hdr: dns.RR_Header{
				Name:   q.Name,
				Rrtype: dns.TypeA,
				Class:  dns.ClassINET,
				Ttl:    60,
			},
			A: fakeIP,
		}
		resp.Answer = append(resp.Answer, rr)
		s.ipToDomain.Store(fakeIP.String(), qName)
	} else {
		// 境外域名走 DoH (SPP) 远端解析
		resp, err = s.resolveDoH(r)
		if err != nil {
			loggo.Warn("[DNS] DoH failed for %s, falling back to direct DNS: %v", qName, err)
			resp, err = s.resolveChinaDNS(r)
		}
	}

	if err != nil || resp == nil {
		dns.HandleFailed(w, r)
		return
	}

	resp.Id = r.Id
	_ = w.WriteMsg(resp)

	// 3. 记录 IP 映射与缓存
	s.cacheAndRecordIP(qName, cacheKey, resp)
}

func (s *Server) resolveChinaDNS(r *dns.Msg) (*dns.Msg, error) {
	c := new(dns.Client)
	c.Timeout = 2 * time.Second
	in, _, err := c.Exchange(r, s.chinaDNS)
	return in, err
}

func (s *Server) resolveDoH(r *dns.Msg) (*dns.Msg, error) {
	data, err := r.Pack()
	if err != nil {
		return nil, err
	}

	s.mu.RLock()
	client := s.httpClientDoH
	doh := s.dohURL
	s.mu.RUnlock()

	req, err := http.NewRequest("POST", doh, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("doh status error: %d", resp.StatusCode)
	}

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	in := new(dns.Msg)
	if err := in.Unpack(respData); err != nil {
		return nil, err
	}
	return in, nil
}

func (s *Server) cacheAndRecordIP(domain, key string, msg *dns.Msg) {
	if len(msg.Answer) == 0 {
		return
	}

	var minTTL uint32 = 300
	for _, ans := range msg.Answer {
		if ans.Header().Ttl < minTTL && ans.Header().Ttl > 0 {
			minTTL = ans.Header().Ttl
		}
		if a, ok := ans.(*dns.A); ok {
			s.ipToDomain.Store(a.A.String(), domain)
		}
		if aaaa, ok := ans.(*dns.AAAA); ok {
			s.ipToDomain.Store(aaaa.AAAA.String(), domain)
		}
	}

	s.cache.Store(key, DNSCacheEntry{
		Msg:       msg.Copy(),
		ExpiresAt: time.Now().Add(time.Duration(minTTL) * time.Second),
	})
}

// LookupDomainByIP 反查 IP 对应的解析域名
func (s *Server) LookupDomainByIP(ip string) (string, bool) {
	if s.fakeIPPool != nil {
		if d, ok := s.fakeIPPool.LookupDomainByIP(ip); ok {
			return d, true
		}
	}
	val, ok := s.ipToDomain.Load(ip)
	if !ok {
		return "", false
	}
	return val.(string), true
}
