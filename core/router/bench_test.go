package router

import (
	"net"
	"testing"
)

func BenchmarkDecideDomain(b *testing.B) {
	r := NewRouterWithOptions(Options{})
	r.AddDirectDomain("baidu.com")
	r.AddProxyDomain("google.com")
	domains := []string{"www.baidu.com", "example.com", "a.b.c.google.com", "cdn.cloudflare.net"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.Decide(domains[i%len(domains)], nil)
	}
}

func BenchmarkDecideIP(b *testing.B) {
	r := NewRouterWithOptions(Options{})
	ips := []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("114.114.114.114"), net.ParseIP("10.0.0.1")}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.Decide("", ips[i%len(ips)])
	}
}
