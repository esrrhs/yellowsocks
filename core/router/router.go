package router

import (
	"bufio"
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/esrrhs/gohome/loggo"
)

//go:embed chnroutes_default.txt
var embeddedChnroutes []byte

// IPNetList CIDR 列表
type IPNetList []*net.IPNet

func (l IPNetList) Contains(ip net.IP) bool {
	for _, ipnet := range l {
		if ipnet.Contains(ip) {
			return true
		}
	}
	return false
}

// RouteDecision 分流判断结果
type RouteDecision int

const (
	Direct RouteDecision = iota
	Proxy
)

// Router 负责境内外 IP、域名与进程判断
type Router struct {
	directIPs atomic.Value // 存放 IPNetList，支持原子无锁热更新
	directHosts sync.Map    // 境内 host 白名单
	proxyHosts  sync.Map    // 境外 host 黑名单
	inspector   *ProcessInspector

	cacheFile string // 本地持久化缓存路径
	updateURL string // 自动更新地址
	stopCh    chan struct{}
}

// Options 路由器配置参数
type Options struct {
	CacheFilePath  string        // chnroutes 本地缓存文件路径 (如 ./chnroutes.txt)
	UpdateURL      string        // 远程更新 URL (留空则使用默认源)
	UpdateInterval time.Duration // 自动更新周期 (如 24 * time.Hour，<=0 不自动更新)
}

// NewRouterWithOptions 创建支持持久化和自动更新的路由分流器
func NewRouterWithOptions(opt Options) *Router {
	if opt.UpdateURL == "" {
		opt.UpdateURL = "https://ispip.clang.cn/all_cn.txt"
	}

	r := &Router{
		cacheFile: opt.CacheFilePath,
		updateURL: opt.UpdateURL,
		stopCh:    make(chan struct{}),
		inspector: NewProcessInspector(),
	}

	// 1. 初始化保留地址与基础规则
	r.initDefaultDirectDomains()

	// 2. 加载 chnroutes (优先读本地缓存，若无则加载内置 embed 数据)
	r.loadChnroutes()

	// 3. 开启后台周期性自动更新
	if opt.UpdateInterval > 0 {
		r.startAutoUpdate(opt.UpdateInterval)
	}

	return r
}

// NewRouter 创建默认路由分流器
func NewRouter() *Router {
	return NewRouterWithOptions(Options{
		CacheFilePath:  "chnroutes.txt",
		UpdateInterval: 24 * time.Hour,
	})
}

func (r *Router) getReservedIPNets() IPNetList {
	reserved := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"224.0.0.0/4",
		"240.0.0.0/4",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
		// 常见国内公共 DNS
		"223.5.5.5/32",
		"223.6.6.6/32",
		"119.29.29.29/32",
		"180.76.76.76/32",
		"114.114.114.114/32",
		"114.114.115.115/32",
	}

	var list IPNetList
	for _, cidr := range reserved {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err == nil {
			list = append(list, ipnet)
		}
	}
	return list
}

func (r *Router) initDefaultDirectDomains() {
	commonDirect := []string{
		"cn",
		"baidu.com",
		"qq.com",
		"taobao.com",
		"aliyun.com",
		"jd.com",
		"bilibili.com",
		"163.com",
		"zhihu.com",
		"weibo.com",
		"tencent.com",
	}
	for _, d := range commonDirect {
		r.directHosts.Store(strings.ToLower(d), true)
	}
}

func (r *Router) parseCIDRReader(reader io.Reader) IPNetList {
	list := r.getReservedIPNets()
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 容错处理纯 IP 或无掩码格式
		if !strings.Contains(line, "/") {
			line = line + "/32"
		}
		_, ipnet, err := net.ParseCIDR(line)
		if err == nil {
			list = append(list, ipnet)
		}
	}
	return list
}

func (r *Router) loadChnroutes() {
	var loaded IPNetList

	// 优先检查外部持久化缓存文件
	if r.cacheFile != "" {
		if f, err := os.Open(r.cacheFile); err == nil {
			defer f.Close()
			loaded = r.parseCIDRReader(f)
			loggo.Info("[Router] Loaded %d IP subnets from local cache %s", len(loaded), r.cacheFile)
		}
	}

	// 本地无缓存则从 embed 读取
	if len(loaded) == 0 {
		loaded = r.parseCIDRReader(bytes.NewReader(embeddedChnroutes))
		loggo.Info("[Router] Loaded %d IP subnets from embedded chnroutes", len(loaded))
	}

	r.directIPs.Store(loaded)
}

// UpdateChnroutesNow 立即从远程获取最新的 chnroutes 并热更新
func (r *Router) UpdateChnroutesNow() error {
	loggo.Info("[Router] Fetching latest chnroutes from %s ...", r.updateURL)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(r.updateURL)
	if err != nil {
		return fmt.Errorf("failed to download chnroutes: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad HTTP status: %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	newList := r.parseCIDRReader(bytes.NewReader(data))
	if len(newList) < 100 { // 基础防呆，避免解析空数据或错误页面
		return fmt.Errorf("downloaded chnroutes too small (%d items), aborted", len(newList))
	}

	// 原子替换内存分流表
	r.directIPs.Store(newList)
	loggo.Info("[Router] Successfully updated chnroutes! Total direct subnets: %d", len(newList))

	// 写入本地持久化文件，供下次启动离线可用
	if r.cacheFile != "" {
		if err := os.WriteFile(r.cacheFile, data, 0644); err != nil {
			loggo.Warn("[Router] Failed to write cache file %s: %v", r.cacheFile, err)
		}
	}

	return nil
}

func (r *Router) startAutoUpdate(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-r.stopCh:
				return
			case <-ticker.C:
				if err := r.UpdateChnroutesNow(); err != nil {
					loggo.Warn("[Router] Auto update chnroutes failed: %v", err)
				}
			}
		}
	}()
}

// Close 停止后台自动更新 goroutine 与进程检查器
func (r *Router) Close() {
	select {
	case <-r.stopCh:
	default:
		close(r.stopCh)
	}
	if r.inspector != nil {
		r.inspector.Close()
	}
}

// AddDirectCIDR 添加直连 CIDR
func (r *Router) AddDirectCIDR(cidr string) error {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return err
	}
	current := r.directIPs.Load().(IPNetList)
	current = append(current, ipnet)
	r.directIPs.Store(current)
	return nil
}

// AddDirectDomain 添加直连域名后缀
func (r *Router) AddDirectDomain(domain string) {
	r.directHosts.Store(strings.ToLower(strings.Trim(domain, ".")), true)
}

// ShouldDirectDomain 判断域名是否应该境内直连
func (r *Router) ShouldDirectDomain(domain string) bool {
	domain = strings.ToLower(strings.Trim(domain, "."))
	if strings.HasSuffix(domain, ".cn") {
		return true
	}
	parts := strings.Split(domain, ".")
	for i := 0; i < len(parts); i++ {
		sub := strings.Join(parts[i:], ".")
		if _, ok := r.directHosts.Load(sub); ok {
			return true
		}
	}
	return false
}

// ShouldDirectIP 判断 IP 是否属于境内/局域网直连
func (r *Router) ShouldDirectIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	val := r.directIPs.Load()
	if val == nil {
		return false
	}
	list := val.(IPNetList)
	return list.Contains(ip)
}

// AddBypassApp 添加排除直连应用
func (r *Router) AddBypassApp(name string) {
	if r.inspector != nil {
		r.inspector.AddBypassApp(name)
	}
}

// Inspector 获取进程检查器
func (r *Router) Inspector() *ProcessInspector {
	return r.inspector
}

// DecideWithPort 增加源端口进程判定
func (r *Router) DecideWithPort(destHost string, destIP net.IP, srcPort int) RouteDecision {
	// 1. 优先检查发起连接的本地进程是否在排除名单中 (TUN Bypass Apps)
	if srcPort > 0 && r.inspector != nil {
		if r.inspector.IsBypassPort(srcPort) {
			return Direct
		}
	}
	// 2. 常规 IP/域名判定
	return r.Decide(destHost, destIP)
}

// Decide 根据目标 host/ip 判定分流目标
func (r *Router) Decide(destHost string, destIP net.IP) RouteDecision {
	if destIP != nil {
		if r.ShouldDirectIP(destIP) {
			return Direct
		}
	}
	if destHost != "" {
		if r.ShouldDirectDomain(destHost) {
			return Direct
		}
	}
	// 默认为境外流量，走代理
	return Proxy
}
