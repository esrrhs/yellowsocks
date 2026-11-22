package dns

import (
	"net"
	"strings"
	"testing"

	"github.com/esrrhs/yellowsocks/core/router"
	"github.com/miekg/dns"
)

// localUDPResolver answers every A query with 8.8.8.8, so a test can tell
// whether a response came from the real upstream (8.8.8.8) or the fake-ip
// pool (198.18.0.0/15).
func localUDPResolver(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			req := new(dns.Msg)
			if err := req.Unpack(buf[:n]); err != nil || len(req.Question) == 0 {
				continue
			}
			resp := new(dns.Msg)
			resp.SetReply(req)
			q := req.Question[0]
			if q.Qtype == dns.TypeA {
				resp.Answer = append(resp.Answer, &dns.A{
					Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
					A:   net.ParseIP("8.8.8.8"),
				})
			}
			packed, err := resp.Pack()
			if err != nil {
				continue
			}
			_, _ = pc.WriteTo(packed, addr)
		}
	}()
	return pc.LocalAddr().String()
}

func aQuery(t *testing.T, name string) []byte {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(name, dns.TypeA)
	b, err := m.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func unpackA(t *testing.T, payload []byte) (string, *dns.Msg) {
	t.Helper()
	m := new(dns.Msg)
	if err := m.Unpack(payload); err != nil {
		t.Fatalf("unpack reply: %v", err)
	}
	if len(m.Answer) == 0 {
		return "", m
	}
	a, ok := m.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("answer is %T, want *dns.A", m.Answer[0])
	}
	return a.A.String(), m
}

func TestFakeIPTUNModeAllocatesAndRecovers(t *testing.T) {
	upstream := localUDPResolver(t)
	rt := router.NewRouter()
	defer rt.Close()
	rt.AddDirectDomain("direct.example")

	s, err := NewServer(Config{
		ListenAddr: "127.0.0.1:0",
		DirectDNS:  upstream,
		Router:     rt,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.EnableTUN(""); err != nil {
		t.Fatalf("EnableTUN: %v", err)
	}
	defer s.DisableTUN()
	if !s.TUNActive() {
		t.Fatal("TUNActive() = false after EnableTUN")
	}

	// 1. A non-direct domain is answered from the fake-ip pool, NOT the
	//    upstream's 8.8.8.8, and the mapping is recoverable.
	ip, _ := unpackA(t, s.ResolvePayload(aQuery(t, "proxy.example.")))
	if ip == "" {
		t.Fatal("no answer for proxy.example")
	}
	if ip == "8.8.8.8" {
		t.Fatalf("proxy.example resolved to upstream IP %s, want a fake-ip address", ip)
	}
	if !s.IsFakeIP(ip) {
		t.Fatalf("IsFakeIP(%q) = false, want true", ip)
	}
	if got := net.ParseIP(ip); got == nil || !strings.HasPrefix(ip, "198.18.") {
		t.Fatalf("fake ip %q not in 198.18.0.0/15", ip)
	}
	if host, ok := s.LookupDomainByIP(ip); !ok || host != "proxy.example" {
		t.Fatalf("LookupDomainByIP(%q) = %q,%v want proxy.example", ip, host, ok)
	}

	// 2. The same domain always gets the same fake address.
	ip2, _ := unpackA(t, s.ResolvePayload(aQuery(t, "proxy.example.")))
	if ip2 != ip {
		t.Fatalf("allocation unstable: %q then %q", ip, ip2)
	}

	// 3. A different domain gets a different fake address.
	ip3, _ := unpackA(t, s.ResolvePayload(aQuery(t, "other.example.")))
	if ip3 == "" || ip3 == ip {
		t.Fatalf("other.example got %q, want a distinct fake address", ip3)
	}

	// 4. Reverse (PTR) lookup of a fake address returns the mapped domain.
	ptrName := ""
	parts := strings.Split(ip, ".")
	if len(parts) == 4 {
		ptrName = parts[3] + "." + parts[2] + "." + parts[1] + "." + parts[0] + ".in-addr.arpa."
	}
	ptr := new(dns.Msg)
	ptr.SetQuestion(ptrName, dns.TypePTR)
	resp, err := s.ResolveMsg(ptr)
	if err != nil || len(resp.Answer) == 0 {
		t.Fatalf("PTR resolve: %v %v", err, resp)
	}
	ptrAns, ok := resp.Answer[0].(*dns.PTR)
	if !ok {
		t.Fatalf("PTR answer is %T", resp.Answer[0])
	}
	if got := strings.TrimSuffix(ptrAns.Ptr, "."); got != "proxy.example" {
		t.Fatalf("PTR = %q, want proxy.example", got)
	}

	// 5. A direct domain still resolves to the upstream's real IP and is NOT
	//    treated as a fake address.
	realIP, _ := unpackA(t, s.ResolvePayload(aQuery(t, "direct.example.")))
	if realIP != "8.8.8.8" {
		t.Fatalf("direct.example = %q, want 8.8.8.8 (real upstream answer)", realIP)
	}
	if s.IsFakeIP(realIP) {
		t.Fatalf("IsFakeIP(%q) = true for a direct-domain real IP", realIP)
	}
	if host, ok := s.LookupDomainByIP(realIP); !ok || host != "direct.example" {
		t.Fatalf("LookupDomainByIP(%q) = %q,%v want direct.example", realIP, host, ok)
	}

	// 6. DisableTUN flips the hijack gate off.
	s.DisableTUN()
	if s.TUNActive() {
		t.Fatal("TUNActive() = true after DisableTUN")
	}
}

func TestFakeIPEnableTUNRejectsBadRange(t *testing.T) {
	s, err := NewServer(Config{ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnableTUN("not-a-cidr"); err == nil {
		t.Fatal("EnableTUN accepted an invalid range")
	}
	// A /30 is smaller than the reserved low block + headroom.
	if err := s.EnableTUN("198.18.0.0/30"); err == nil {
		t.Fatal("EnableTUN accepted a too-small range")
	}
	if s.TUNActive() {
		t.Fatal("TUNActive after failed EnableTUN")
	}
}
