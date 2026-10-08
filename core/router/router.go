package router

import (
	"bufio"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	gohomedns "github.com/esrrhs/gohome/dns"
	"github.com/esrrhs/gohome/dns/matcher"
	"github.com/esrrhs/gohome/loggo"
)

// Built-in direct domains, TLD suffixes and reserved CIDRs are maintained in
// one place: github.com/esrrhs/gohome/dns defaults. Update them there, bump the
// dependency, and every consumer (including yellowsocks) picks up the change.

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
}

// Options router configuration options
type Options struct {
	DirectDomains    []string // custom direct domain suffixes
	ProxyDomains     []string // custom proxy domain suffixes
	DirectCIDRs      []string // custom direct CIDR subnets
	DisableBuiltin   bool     // skip built-in direct domains; caller supplies Rules
	GeoIPFile        string   // GeoLite2 mmdb file path
	ChinaDomainFiles []string // custom direct domain files (e.g. accelerated-domains.china.conf)
	GFWDomainFiles   []string // custom proxy domain files
	SkipCountry      string   // skip country ISO code (default "CN")
}

// NewRouterWithOptions creates a router with custom domains, CIDRs and GeoIP.
func NewRouterWithOptions(opt Options) *Router {
	skipCountry := opt.SkipCountry
	if skipCountry == "" {
		skipCountry = "CN"
	}

	r := &Router{
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
		for _, d := range gohomedns.DefaultChinaMainDomains {
			r.AddDirectDomain(d)
		}
		for _, d := range gohomedns.DefaultDirectTLDs {
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

	// Load custom direct CIDRs on top of the reserved ranges.
	r.loadDirectCIDRs(opt.DirectCIDRs)

	return r
}

// NewRouter creates a default router
func NewRouter() *Router {
	return NewRouterWithOptions(Options{})
}

func (r *Router) getReservedIPNets() IPNetList {
	// Canonical list is maintained in gohome/dns: private, loopback,
	// link-local, CGNAT, multicast and reserved ranges for IPv4/IPv6.
	var list IPNetList
	for _, cidr := range gohomedns.DefaultReservedCIDRs {
		if _, ipnet, err := net.ParseCIDR(cidr); err == nil {
			list = append(list, ipnet)
		}
	}
	return list
}

// loadDirectCIDRs installs reserved subnets plus the configured custom CIDRs.
func (r *Router) loadDirectCIDRs(customCIDRs []string) {
	list := r.getReservedIPNets()
	for _, cidr := range customCIDRs {
		if _, ipnet, err := net.ParseCIDR(cidr); err == nil {
			list = append(list, ipnet)
		}
	}
	r.directIPs.Store(list)
}

// Close releases the GeoIP database.
func (r *Router) Close() {
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
