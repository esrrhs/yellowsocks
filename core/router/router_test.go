package router

import (
	"net"
	"testing"
)

func TestRouterDecidePrivateIPs(t *testing.T) {
	r := NewRouter()
	defer r.Close()

	directIPs := []string{
		"10.0.0.1",
		"10.255.0.2",
		"172.16.1.1",
		"172.31.255.254",
		"192.168.1.1",
		"192.168.100.200",
		"127.0.0.1",
		"169.254.1.1",
	}

	for _, ipStr := range directIPs {
		ip := net.ParseIP(ipStr)
		decision := r.Decide("", ip)
		if decision != Direct {
			t.Errorf("expected Direct for private/reserved IP %s, got %v", ipStr, decision)
		}
	}
}

func TestRouterDecidePublicIPs(t *testing.T) {
	r := NewRouter()
	defer r.Close()

	publicIPs := []string{
		"1.1.1.1",
		"8.8.8.8",
		"140.82.121.4",
		"151.101.1.69",
	}

	for _, ipStr := range publicIPs {
		ip := net.ParseIP(ipStr)
		decision := r.Decide("", ip)
		if decision != Proxy {
			t.Errorf("expected Proxy for public destination IP %s, got %v", ipStr, decision)
		}
	}
}

func TestRouterCustomDirectDomains(t *testing.T) {
	opt := Options{
		DirectDomains: []string{
			"example.com",
			"internal.corp",
		},
	}
	r := NewRouterWithOptions(opt)
	defer r.Close()

	// Exact domain match
	if !r.ShouldDirectDomain("example.com") {
		t.Errorf("expected example.com to be direct")
	}

	// Subdomain match (wildcard suffix)
	if !r.ShouldDirectDomain("api.example.com") {
		t.Errorf("expected api.example.com to be direct")
	}
	if !r.ShouldDirectDomain("dev.mail.internal.corp") {
		t.Errorf("expected dev.mail.internal.corp to be direct")
	}

	// Unmatched domain
	if r.ShouldDirectDomain("notexample.com") {
		t.Errorf("expected notexample.com to NOT be direct")
	}
	if r.ShouldDirectDomain("google.com") {
		t.Errorf("expected google.com to NOT be direct")
	}

	// Decision with domain
	if r.Decide("api.example.com", net.ParseIP("1.2.3.4")) != Direct {
		t.Errorf("expected Direct decision for whitelisted domain")
	}
}

func TestRouterCustomDirectCIDR(t *testing.T) {
	r := NewRouter()
	defer r.Close()

	customCIDR := "203.0.113.0/24"
	if err := r.AddDirectCIDR(customCIDR); err != nil {
		t.Fatalf("failed to add direct CIDR: %v", err)
	}

	target := net.ParseIP("203.0.113.50")
	if r.Decide("", target) != Direct {
		t.Errorf("expected Direct for custom CIDR IP %s", target)
	}

	outsideTarget := net.ParseIP("203.0.114.50")
	if r.Decide("", outsideTarget) != Proxy {
		t.Errorf("expected Proxy for IP outside custom CIDR %s", outsideTarget)
	}
}

func TestRouterChinaSuffixesAndProxyDomains(t *testing.T) {
	r := NewRouter()
	defer r.Close()

	r.AddProxyDomain("blocked.com")

	// CN suffixes should direct
	if !r.ShouldDirectDomain("baidu.com.cn") {
		t.Errorf("expected baidu.com.cn to direct")
	}
	if !r.ShouldDirectDomain("gov.cn") {
		t.Errorf("expected gov.cn to direct")
	}
	if r.Decide("news.sina.com.cn", net.ParseIP("1.2.3.4")) != Direct {
		t.Errorf("expected Direct for .cn domain")
	}

	// GFW/Proxy domain should proxy even if IP is omitted
	if !r.ShouldProxyDomain("sub.blocked.com") {
		t.Errorf("expected sub.blocked.com to proxy")
	}
	if r.Decide("sub.blocked.com", nil) != Proxy {
		t.Errorf("expected Proxy for sub.blocked.com")
	}
}
