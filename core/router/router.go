package router

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/esrrhs/gohome/dns/matcher"
	"github.com/esrrhs/gohome/loggo"
)

var defaultCNSuffixes = []string{
	".cn",
	".xn--fiqs8s", // 中国
	".xn--55qx5d", // 公司
	".xn--io0a7i", // 网络
}

// builtinDirectDomains are common Chinese sites and domestic IP lookups.
// They stay off Fake-IP so their latency is the direct path.
var builtinDirectDomains = []string{
	"baidu.com",
	"qq.com",
	"taobao.com",
	"bilibili.com",
	"jd.com",
	"163.com",
	"alipay.com",
	"aliyun.com",
	"weibo.com",
	"iqiyi.com",
	"youku.com",
	"zhihu.com",
	"ipip.net",
	"3322.net",
	"alicdn.com",
	"gtimg.com",
	"bdstatic.com",
	"hdslb.com",
}

// IPNetList represents a list of CIDR network subnets
type IPNetList []*net.IPNet

func (l IPNetList) Contains(ip net.IP) bool {
	for _, ipnet := range l {
		if ipnet.Contains(ip) {
			return true
		}
	}
	return false
}

// RouteDecision routing decision result
type RouteDecision int

const (
	Direct RouteDecision = iota
	Proxy
)

// Router handles IP and domain routing decisions.
type Router struct {
	directIPs   atomic.Value // holds IPNetList, supporting atomic lock-free hot swapping
	directHosts sync.Map     // direct host whitelist
	proxyHosts  sync.Map     // proxy host list
	geoDB       *matcher.GeoDB
	skipCountry string

	cacheFile string // local persistent cache path
	updateURL string // remote routes update URL
	stopCh    chan struct{}
}

// Options router configuration options
type Options struct {
	DirectRoutesFile string        // custom direct routes file (e.g. routes.txt)
	UpdateURL        string        // custom routes update URL
	UpdateInterval   time.Duration // auto-update interval (<=0 disables auto update)
	DirectDomains    []string      // custom direct domain suffixes
	ProxyDomains     []string      // custom proxy domain suffixes
	DirectCIDRs      []string      // custom direct CIDR subnets
	DisableBuiltin   bool          // skip built-in direct domains; caller supplies Rules
	GeoIPFile        string        // GeoLite2 mmdb file path
	ChinaDomainFiles []string      // custom direct domain files (e.g. accelerated-domains.china.conf)
	GFWDomainFiles   []string      // custom proxy domain files
	SkipCountry      string        // skip country ISO code (default "CN")
}

// NewRouterWithOptions creates a router supporting custom routes, caching, and auto-update
func NewRouterWithOptions(opt Options) *Router {
	skipCountry := opt.SkipCountry
	if skipCountry == "" {
		skipCountry = "CN"
	}

	r := &Router{
		cacheFile:   opt.DirectRoutesFile,
		updateURL:   opt.UpdateURL,
		stopCh:      make(chan struct{}),
		skipCountry: skipCountry,
		geoDB:       matcher.NewGeoDB(),
	}

	if opt.GeoIPFile != "" {
		if err := r.geoDB.Open(opt.GeoIPFile); err != nil {
			loggo.Warn("[Router] Failed to load GeoIP file %s: %v", opt.GeoIPFile, err)
		} else {
			loggo.Info("[Router] Loaded GeoIP file: %s", opt.GeoIPFile)
		}
	} else if _, err := os.Stat("GeoLite2-Country.mmdb"); err == nil {
		if err := r.geoDB.Open("GeoLite2-Country.mmdb"); err == nil {
			loggo.Info("[Router] Loaded default GeoIP file: GeoLite2-Country.mmdb")
		}
	}

	// Initialize reserved subnets and configured domains.
	if !opt.DisableBuiltin {
		for _, d := range builtinDirectDomains {
			r.AddDirectDomain(d)
		}
		for _, d := range defaultCNSuffixes {
			r.AddDirectDomain(d)
		}
	}
	for _, d := range opt.DirectDomains {
		r.AddDirectDomain(d)
	}
	for _, d := range opt.ProxyDomains {
		r.AddProxyDomain(d)
	}

	// Load domain files
	for _, f := range opt.ChinaDomainFiles {
		_ = r.LoadDomainFile(f, true)
	}
	for _, f := range opt.GFWDomainFiles {
		_ = r.LoadDomainFile(f, false)
	}

	// 2. Load custom direct routes
	r.loadRoutes(opt.DirectCIDRs)

	// 3. Start periodic auto-updater if URL and interval are provided
	if opt.UpdateURL != "" && opt.UpdateInterval > 0 {
		r.startAutoUpdate(opt.UpdateInterval)
	}

	return r
}

// NewRouter creates a default router
func NewRouter() *Router {
	return NewRouterWithOptions(Options{
		DirectRoutesFile: "direct_routes.txt",
		UpdateInterval:   0,
	})
}

func (r *Router) getReservedIPNets() IPNetList {
	// Standard RFC private, loopback and link-local ranges
	reserved := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"224.0.0.0/4",
		"240.0.0.0/4",
		"::/128",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
		"ff00::/8",
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

func (r *Router) parseCIDRReader(reader io.Reader) IPNetList {
	list := r.getReservedIPNets()
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
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

func (r *Router) loadRoutes(customCIDRs []string) {
	var loaded IPNetList

	// Load from local cache file if available
	if r.cacheFile != "" {
		if f, err := os.Open(r.cacheFile); err == nil {
			defer f.Close()
			loaded = r.parseCIDRReader(f)
			loggo.Info("[Router] Loaded %d direct IP subnets from cache %s", len(loaded), r.cacheFile)
		}
	}

	if len(loaded) == 0 {
		loaded = r.getReservedIPNets()
	}

	for _, cidr := range customCIDRs {
		if _, ipnet, err := net.ParseCIDR(cidr); err == nil {
			loaded = append(loaded, ipnet)
		}
	}

	r.directIPs.Store(loaded)
}

// UpdateRoutesNow downloads routes from remote URL and hot-replaces the routing table
func (r *Router) UpdateRoutesNow() error {
	if r.updateURL == "" {
		return fmt.Errorf("no update URL configured")
	}

	loggo.Info("[Router] Fetching latest direct routes from %s ...", r.updateURL)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(r.updateURL)
	if err != nil {
		return fmt.Errorf("failed to download routes: %w", err)
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
	if len(newList) < 10 {
		return fmt.Errorf("downloaded routes too small (%d items), aborted", len(newList))
	}

	r.directIPs.Store(newList)
	loggo.Info("[Router] Successfully updated direct routes! Total direct subnets: %d", len(newList))

	if r.cacheFile != "" {
		_ = os.WriteFile(r.cacheFile, data, 0644)
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
				if err := r.UpdateRoutesNow(); err != nil {
					loggo.Warn("[Router] Auto update routes failed: %v", err)
				}
			}
		}
	}()
}

// Close stops background routines
func (r *Router) Close() {
	select {
	case <-r.stopCh:
	default:
		close(r.stopCh)
	}
	if r.geoDB != nil {
		_ = r.geoDB.Close()
	}
}

// AddDirectCIDR adds a direct CIDR subnet
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

// AddDirectDomain adds a direct domain suffix
func (r *Router) AddDirectDomain(domain string) {
	d := strings.ToLower(strings.Trim(domain, "."))
	if d != "" {
		r.directHosts.Store(d, true)
	}
}

// AddProxyDomain adds a proxy domain suffix
func (r *Router) AddProxyDomain(domain string) {
	d := strings.ToLower(strings.Trim(domain, "."))
	if d != "" {
		r.proxyHosts.Store(d, true)
	}
}

// LoadDomainFile loads a domain list file (supports dnsmasq format server=/domain/... or plain domains)
func (r *Router) LoadDomainFile(filePath string, isDirect bool) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	count := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "server=/"); ok {
			domain, _, found := strings.Cut(rest, "/")
			if found && domain != "" {
				line = domain
			}
		}
		line = strings.ToLower(strings.Trim(line, "."))
		if line != "" {
			if isDirect {
				r.AddDirectDomain(line)
			} else {
				r.AddProxyDomain(line)
			}
			count++
		}
	}
	loggo.Info("[Router] Loaded %d domain rules from %s (direct: %v)", count, filePath, isDirect)
	return scanner.Err()
}

// ShouldProxyDomain checks if domain matches proxy domain list
func (r *Router) ShouldProxyDomain(domain string) bool {
	return matchListed(&r.proxyHosts, domain)
}

// ShouldDirectDomain checks if domain matches direct whitelist or suffix rules.
func (r *Router) ShouldDirectDomain(domain string) bool {
	return matchListed(&r.directHosts, domain)
}

func matchListed(hosts *sync.Map, domain string) bool {
	domain = strings.ToLower(strings.Trim(domain, "."))
	if domain == "" {
		return false
	}
	// Label-boundary suffix match, not substring and not regexp.
	// baidu.com matches www.baidu.com, not notbaidu.com.
	// A leading-dot rule (.cn) is stored as cn and matches that whole label.
	parts := strings.Split(domain, ".")
	for i := 0; i < len(parts); i++ {
		sub := strings.Join(parts[i:], ".")
		if _, ok := hosts.Load(sub); ok {
			return true
		}
		if _, ok := hosts.Load("." + sub); ok {
			return true
		}
	}
	return false
}

// canonicalIP turns IPv4 and IPv4-mapped IPv6 into a 4-byte address so the
// same CIDR and GeoIP rules apply to both spellings.
func canonicalIP(ip net.IP) net.IP {
	if ip == nil {
		return nil
	}
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
}

// ShouldDirectIP checks if IP matches direct subnet rules
func (r *Router) ShouldDirectIP(ip net.IP) bool {
	ip = canonicalIP(ip)
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

// Decide determines route action based on host and destination IP
func (r *Router) Decide(destHost string, destIP net.IP) RouteDecision {
	destIP = canonicalIP(destIP)
	if ip := net.ParseIP(destHost); ip != nil {
		destIP = canonicalIP(ip)
		destHost = ""
	}

	// If domain is specified, evaluate domain rules first
	if destHost != "" {
		if r.ShouldProxyDomain(destHost) {
			return Proxy
		}
		if r.ShouldDirectDomain(destHost) {
			return Direct
		}
	}

	// Evaluate IP rules
	if destIP != nil {
		if r.ShouldDirectIP(destIP) {
			return Direct
		}
		if r.geoDB != nil && r.skipCountry != "" {
			if r.geoDB.IsCountry(destIP, r.skipCountry) {
				return Direct
			}
		}
	}

	return Proxy
}
