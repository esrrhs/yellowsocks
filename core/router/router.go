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

// hostSet is a set of domain suffix rules.
//
// Reads happen on the hot path: every routed connection, every UDP datagram
// and every DNS query looks a host up. Writes happen only when rules are
// loaded. The set is therefore copied on write and read through an atomic
// pointer, which makes a lookup a plain map access: no lock, and no
// interface boxing (sync.Map.Load would allocate on every call).
type hostSet struct {
	mu   sync.Mutex
	rule atomic.Pointer[map[string]struct{}]
}

func newHostSet() *hostSet {
	h := &hostSet{}
	h.reset()
	return h
}

func (h *hostSet) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := make(map[string]struct{})
	h.rule.Store(&m)
}

// add publishes one suffix. Leading and trailing dots are trimmed, so rules
// are matched by label boundary and never contain a dot themselves.
func (h *hostSet) add(domain string) {
	h.addAll([]string{domain})
}

// addAll publishes a batch of suffixes with one copy of the set. Copying per
// suffix would be quadratic, and the built-in China domain list alone is
// tens of thousands of entries long.
func (h *hostSet) addAll(domains []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	old := h.rule.Load()
	m := make(map[string]struct{}, len(*old)+len(domains))
	for k := range *old {
		m[k] = struct{}{}
	}
	added := 0
	for _, domain := range domains {
		d := strings.ToLower(strings.Trim(domain, "."))
		if d == "" {
			continue
		}
		m[d] = struct{}{}
		added++
	}
	if added == 0 {
		return
	}
	h.rule.Store(&m)
}

func (h *hostSet) has(domain string) bool {
	m := h.rule.Load()
	if m == nil {
		return false
	}
	_, ok := (*m)[domain]
	return ok
}

func (h *hostSet) len() int {
	m := h.rule.Load()
	if m == nil {
		return 0
	}
	return len(*m)
}

// Router handles IP and domain routing decisions.
type Router struct {
	directIPs   atomic.Value // holds IPNetList, supporting atomic lock-free hot swapping
	directHosts *hostSet     // direct host whitelist
	proxyHosts  *hostSet     // proxy host list
	geoDB       *matcher.GeoDB
	geoipLoaded bool // GeoIP database successfully opened
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
		directHosts: newHostSet(),
		proxyHosts:  newHostSet(),
	}

	if opt.GeoIPFile != "" {
		if err := r.geoDB.Open(opt.GeoIPFile); err != nil {
			loggo.Warn("[Router] Failed to load GeoIP file %s: %v", opt.GeoIPFile, err)
		} else {
			r.geoipLoaded = true
			loggo.Info("[Router] Loaded GeoIP file: %s", opt.GeoIPFile)
		}
	} else if _, err := os.Stat("GeoLite2-Country.mmdb"); err == nil {
		if err := r.geoDB.Open("GeoLite2-Country.mmdb"); err == nil {
			r.geoipLoaded = true
			loggo.Info("[Router] Loaded default GeoIP file: GeoLite2-Country.mmdb")
		}
	}

	// Initialize reserved subnets and configured domains.
	if !opt.DisableBuiltin {
		r.directHosts.addAll(gohomedns.DefaultChinaMainDomains)
		r.directHosts.addAll(gohomedns.DefaultDirectTLDs)
	}
	r.directHosts.addAll(opt.DirectDomains)
	r.proxyHosts.addAll(opt.ProxyDomains)

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
	r.directHosts.add(domain)
}

// AddProxyDomain adds a proxy domain suffix
func (r *Router) AddProxyDomain(domain string) {
	r.proxyHosts.add(domain)
}

// LoadDomainFile loads a domain list file (supports dnsmasq format server=/domain/... or plain domains)
func (r *Router) LoadDomainFile(filePath string, isDirect bool) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var lines []string
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
			lines = append(lines, line)
		}
	}
	if isDirect {
		r.directHosts.addAll(lines)
	} else {
		r.proxyHosts.addAll(lines)
	}
	loggo.Info("[Router] Loaded %d domain rules from %s (direct: %v)", len(lines), filePath, isDirect)
	return scanner.Err()
}

// ShouldProxyDomain checks if domain matches proxy domain list
func (r *Router) ShouldProxyDomain(domain string) bool {
	return matchListed(r.proxyHosts, domain)
}

// ShouldDirectDomain checks if domain matches direct whitelist or suffix rules.
func (r *Router) ShouldDirectDomain(domain string) bool {
	return matchListed(r.directHosts, domain)
}

func matchListed(hosts *hostSet, domain string) bool {
	domain = strings.ToLower(strings.Trim(domain, "."))
	if domain == "" {
		return false
	}
	// Label-boundary suffix match, not substring and not regexp:
	// baidu.com matches www.baidu.com, not notbaidu.com.
	// A leading-dot rule (.cn) is stored as cn and matches that whole label.
	//
	// The suffixes are walked on the '.' boundaries of the original string
	// instead of being split and re-joined: a domain decision is taken on
	// every connection, every UDP datagram and every DNS query, and building
	// a []string plus a string per suffix allocated on each of them.
	for i := 0; i < len(domain); {
		if hosts.has(domain[i:]) {
			return true
		}
		dot := strings.IndexByte(domain[i:], '.')
		if dot < 0 {
			break
		}
		i += dot + 1
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

// Info reports the size of the loaded rule sets. Counts are computed on
// demand: they are only read by the status API, never on the routing path.
type Info struct {
	DirectDomains int
	ProxyDomains  int
	DirectCIDRs   int
	GeoIP         bool
	SkipCountry   string
}

// Info returns a snapshot of the currently loaded rules.
func (r *Router) Info() Info {
	info := Info{SkipCountry: r.skipCountry}
	info.DirectDomains = r.directHosts.len()
	info.ProxyDomains = r.proxyHosts.len()
	if v := r.directIPs.Load(); v != nil {
		info.DirectCIDRs = len(v.(IPNetList))
	}
	info.GeoIP = r.geoipLoaded
	return info
}

// looksLikeIP is a cheap pre-filter in front of net.ParseIP, which allocates
// before it can fail. An IPv6 literal always contains a colon and an IPv4
// literal always starts and ends with a digit, so a host name can be
// rejected without parsing. A false positive only costs one parse; a false
// negative would be a routing bug, so the test is deliberately loose.
func looksLikeIP(s string) bool {
	if s == "" {
		return false
	}
	if strings.IndexByte(s, ':') >= 0 {
		return true
	}
	return isDigit(s[0]) && isDigit(s[len(s)-1])
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// Decide determines route action based on host and destination IP
func (r *Router) Decide(destHost string, destIP net.IP) RouteDecision {
	destIP = canonicalIP(destIP)
	if looksLikeIP(destHost) {
		if ip := net.ParseIP(destHost); ip != nil {
			destIP = canonicalIP(ip)
			destHost = ""
		}
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
