package dns

import (
	"fmt"
	"net"
	"sync"
	"testing"
)

func TestFakeIPPoolAllocateAndLookup(t *testing.T) {
	pool := NewFakeIPPool()

	domain1 := "example.com"
	ip1 := pool.Allocate(domain1)
	if ip1 == nil {
		t.Fatalf("expected non-nil IP for %s", domain1)
	}

	if !IsFakeIP(ip1) {
		t.Errorf("expected %s to be recognized as fake IP", ip1.String())
	}

	// Idempotency check: allocating same domain must return identical IP
	ip1Repeat := pool.Allocate(domain1)
	if !ip1.Equal(ip1Repeat) {
		t.Errorf("expected idempotent allocation: %s != %s", ip1, ip1Repeat)
	}

	// Reverse lookup test
	foundDomain, ok := pool.LookupDomainByIP(ip1.String())
	if !ok || foundDomain != domain1 {
		t.Errorf("expected reverse lookup %s, got %s (ok=%v)", domain1, foundDomain, ok)
	}

	// Another domain should receive a distinct IP
	domain2 := "google.com"
	ip2 := pool.Allocate(domain2)
	if ip1.Equal(ip2) {
		t.Errorf("expected different IP for different domain, got %s for both", ip1)
	}

	// Non-existent IP lookup
	_, ok = pool.LookupDomainByIP("1.2.3.4")
	if ok {
		t.Errorf("expected false for unallocated IP")
	}
}

func TestIsFakeIP(t *testing.T) {
	cases := []struct {
		ip       string
		expected bool
	}{
		{"198.18.0.1", true},
		{"198.18.254.10", true},
		{"198.19.1.50", true},
		{"198.20.0.1", false},
		{"192.168.1.1", false},
		{"10.0.0.1", false},
		{"8.8.8.8", false},
		{"127.0.0.1", false},
	}

	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		actual := IsFakeIP(ip)
		if actual != c.expected {
			t.Errorf("IsFakeIP(%s) = %v, expected %v", c.ip, actual, c.expected)
		}
	}

	if IsFakeIP(nil) {
		t.Errorf("expected nil IP to return false")
	}
	if IsFakeIP(net.ParseIP("2001:db8::1")) {
		t.Errorf("expected IPv6 to return false")
	}
}

func TestFakeIPPoolConcurrency(t *testing.T) {
	pool := NewFakeIPPool()
	var wg sync.WaitGroup
	workers := 50
	domainsPerWorker := 40

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < domainsPerWorker; i++ {
				d := fmt.Sprintf("test-%d-%d.com", workerID, i)
				ip := pool.Allocate(d)
				if !IsFakeIP(ip) {
					t.Errorf("concurrency: invalid fake IP %v for %s", ip, d)
				}
				dLookup, ok := pool.LookupDomainByIP(ip.String())
				if !ok || dLookup != d {
					t.Errorf("concurrency lookup mismatch: expected %s, got %s", d, dLookup)
				}
			}
		}(w)
	}
	wg.Wait()
}
