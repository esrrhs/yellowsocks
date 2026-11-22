package dns

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/miekg/dns"
)

// DefaultFakeIPRange is the RFC 2544 benchmarking range Clash/mihomo use for
// fake-ip. It is unroutable on the real Internet, so a handed-out address can
// never collide with a real destination, and it is absent from the router's
// reserved-CIDR direct list, so fake addresses are never misrouted direct.
const DefaultFakeIPRange = "198.18.0.0/15"

// fakeIPReservedLow skips the first 1024 addresses of the pool: the TUN
// adapter lives at 198.18.0.1/30 (gateway peer 198.18.0.2) and sing-tun
// reserves the low block for gateway/neighbor bookkeeping, so assigned fake
// addresses must never overlap the virtual interface itself.
const fakeIPReservedLow = 1024

// fakeIPTTL is how long a synthetic A record is handed to clients. The
// reverse mapping (ipToDomain, ipMappingTTL) outlives it, so a connection
// opened just as the client's cache expires still recovers its domain.
const fakeIPTTL = 600

// EnableTUN switches the engine into TUN-mode DNS handling:
//
//   - every captured :53 datagram is answered in-process by the smart
//     resolver instead of being blindly relayed to a public resolver or the
//     upstream, so domain<->IP mappings are learned system-wide;
//   - proxied domains receive a fake address from the pool, so when the
//     client connects to it the engine recovers the original domain and
//     forwards that name to the upstream SOCKS5, which resolves it remotely
//     — bypassing both GFW DNS pollution and real-IP blocking.
//
// fakeIPRange may be empty to use DefaultFakeIPRange. Safe to call repeatedly
// and without restarting the engine.
func (s *Server) EnableTUN(fakeIPRange string) error {
	if strings.TrimSpace(fakeIPRange) == "" {
		fakeIPRange = DefaultFakeIPRange
	}
	_, ipnet, err := net.ParseCIDR(fakeIPRange)
	if err != nil {
		return fmt.Errorf("invalid fake-ip range %q: %w", fakeIPRange, err)
	}
	ones, bits := ipnet.Mask.Size()
	if bits == 0 {
		return fmt.Errorf("invalid fake-ip range %q: zero-length mask", fakeIPRange)
	}
	hostBits := bits - ones
	if hostBits >= 64 {
		return fmt.Errorf("fake-ip range %q too large", fakeIPRange)
	}
	count := uint64(1) << uint(hostBits)
	if count <= fakeIPReservedLow+16 {
		return fmt.Errorf("fake-ip range %q too small (need more than %d addresses)", fakeIPRange, fakeIPReservedLow+16)
	}

	s.fakeIPMu.Lock()
	s.fakeIPNet = &net.IPNet{IP: ipnet.IP.Mask(ipnet.Mask), Mask: ipnet.Mask}
	s.fakeIPBase = ipnet.IP.Mask(ipnet.Mask)
	s.fakeIPCount = count
	s.fakeIPOffset = fakeIPReservedLow
	s.fakeIPNext = 0
	s.fakeIPMu.Unlock()

	s.fakeIPEnabled.Store(true)
	s.tunMode.Store(true)
	loggo.Info("[DNS] TUN mode on: in-process :53 hijack + fake-ip pool %s (%d usable addresses)",
		fakeIPRange, count-fakeIPReservedLow)
	return nil
}

// DisableTUN reverts EnableTUN: port-53 traffic is no longer intercepted and
// fake-ip allocation stops. Existing mappings expire on their own.
func (s *Server) DisableTUN() {
	if s.tunMode.CompareAndSwap(true, false) {
		s.fakeIPEnabled.Store(false)
		loggo.Info("[DNS] TUN mode off")
	}
}

// TUNActive reports whether the proxy layer should hijack captured :53 traffic.
func (s *Server) TUNActive() bool { return s.tunMode.Load() }

// IsFakeIP reports whether ipStr is an address handed out by the fake-ip pool.
func (s *Server) IsFakeIP(ipStr string) bool {
	if !s.fakeIPEnabled.Load() {
		return false
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	s.fakeIPMu.Lock()
	n := s.fakeIPNet
	s.fakeIPMu.Unlock()
	if n == nil {
		return false
	}
	return n.Contains(ip)
}

// fakeIPForDomain returns the fake IP string assigned to domain, allocating a
// fresh address on first use. The same domain always maps to the same fake IP
// for the lifetime of the pool, so the client's cached answer stays consistent
// with the routing reverse-map.
func (s *Server) fakeIPForDomain(domain string) string {
	if v, ok := s.domainToFakeIP.Load(domain); ok {
		return v.(string)
	}

	s.fakeIPMu.Lock()
	// Double-check after acquiring the lock: a concurrent allocator for the
	// same domain may have won the race.
	if v, ok := s.domainToFakeIP.Load(domain); ok {
		s.fakeIPMu.Unlock()
		return v.(string)
	}
	usable := s.fakeIPCount - s.fakeIPOffset
	idx := s.fakeIPOffset + (s.fakeIPNext % usable)
	s.fakeIPNext++
	base := s.fakeIPBase
	s.fakeIPMu.Unlock()

	ipStr := addOffsetIP(base, idx).String()

	s.domainToFakeIP.Store(domain, ipStr)
	now := time.Now()
	s.ipToDomain.Store(ipStr, ipMapping{domain: domain, expires: now.Add(ipMappingTTL)})
	return ipStr
}

// addOffsetIP returns base + idx as a new net.IP, handling both IPv4 and IPv6
// pools (IPv6 is not used by the default range but the math is kept correct).
func addOffsetIP(base net.IP, idx uint64) net.IP {
	out := make(net.IP, len(base))
	copy(out, base)
	if v4 := out.To4(); v4 != nil {
		n := binary.BigEndian.Uint32(v4)
		n += uint32(idx)
		binary.BigEndian.PutUint32(v4, n)
		return v4
	}
	low := binary.BigEndian.Uint64(out[8:])
	low += idx
	binary.BigEndian.PutUint64(out[8:], low)
	return out
}

// fakeIPReply builds a NOERROR answer carrying one synthetic A record for the
// assigned fake address. No upstream lookup is performed: the upstream SOCKS5
// resolves the real address when the engine forwards the domain name.
func (s *Server) fakeIPReply(r *dns.Msg, qName string) *dns.Msg {
	ip := net.ParseIP(s.fakeIPForDomain(qName))
	resp := new(dns.Msg)
	resp.SetReply(r)
	resp.Authoritative = true
	resp.Answer = append(resp.Answer, &dns.A{
		Hdr: dns.RR_Header{
			Name:   r.Question[0].Name,
			Rrtype: dns.TypeA,
			Class:  dns.ClassINET,
			Ttl:    fakeIPTTL,
		},
		A: ip,
	})
	return resp
}

// fakeIPPtrReply answers a reverse (PTR) query for one of our own fake
// addresses with the mapped domain, instead of leaking the query upstream.
func (s *Server) fakeIPPtrReply(r *dns.Msg, qName string) (*dns.Msg, bool) {
	ip := ptrNameToIP(qName)
	if ip == nil {
		return nil, false
	}
	if !s.IsFakeIP(ip.String()) {
		return nil, false
	}
	domain, ok := s.LookupDomainByIP(ip.String())
	if !ok {
		return nil, false
	}
	resp := new(dns.Msg)
	resp.SetReply(r)
	resp.Authoritative = true
	resp.Answer = append(resp.Answer, &dns.PTR{
		Hdr: dns.RR_Header{
			Name:   r.Question[0].Name,
			Rrtype: dns.TypePTR,
			Class:  dns.ClassINET,
			Ttl:    fakeIPTTL,
		},
		Ptr: dns.Fqdn(domain),
	})
	return resp, true
}

// ptrNameToIP parses an in-addr.arpa name (e.g. "4.0.18.198.in-addr.arpa")
// into the address it names. ip6.arpa is not handled (IPv6 fake-ip is unused).
func ptrNameToIP(name string) net.IP {
	n := strings.ToLower(strings.Trim(name, "."))
	const suffix = ".in-addr.arpa"
	if !strings.HasSuffix(n, suffix) {
		return nil
	}
	body := strings.TrimSuffix(n, suffix)
	parts := strings.Split(body, ".")
	if len(parts) != 4 {
		return nil
	}
	return net.ParseIP(parts[3] + "." + parts[2] + "." + parts[1] + "." + parts[0])
}

// ResolvePayload parses a bare DNS wire query, resolves it through the same
// caching + smart-routing path as the listeners, and returns the packed reply
// with the request Id copied in. A nil return means the payload could not be
// parsed; the caller should drop it. Used by the proxy layer to answer
// captured :53 datagrams in-process.
func (s *Server) ResolvePayload(payload []byte) []byte {
	req := new(dns.Msg)
	if err := req.Unpack(payload); err != nil {
		return nil
	}
	resp, err := s.ResolveMsg(req)
	if err != nil || resp == nil {
		resp = new(dns.Msg)
		resp.SetRcode(req, dns.RcodeServerFailure)
	}
	resp.Id = req.Id
	out, err := resp.Pack()
	if err != nil {
		return nil
	}
	return out
}

// ServeConn answers length-prefixed (RFC 1035 TCP framing) DNS queries on an
// already-accepted connection until EOF or error. It is used to hijack
// captured TCP/53 connections in-process so they never leave the engine.
func (s *Server) ServeConn(conn net.Conn) error {
	defer conn.Close()
	for {
		var lbuf [2]byte
		if _, err := io.ReadFull(conn, lbuf[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		length := binary.BigEndian.Uint16(lbuf[:])
		if length == 0 {
			return errors.New("zero-length DNS message")
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return err
		}
		out := s.ResolvePayload(buf)
		if out == nil {
			// Always emit a reply so the client does not stall.
			fail := new(dns.Msg)
			if req := new(dns.Msg); req.Unpack(buf) == nil {
				fail.SetRcode(req, dns.RcodeServerFailure)
				fail.Id = req.Id
			}
			out, _ = fail.Pack()
		}
		var outLen [2]byte
		binary.BigEndian.PutUint16(outLen[:], uint16(len(out)))
		if _, err := conn.Write(outLen[:]); err != nil {
			return err
		}
		if _, err := conn.Write(out); err != nil {
			return err
		}
	}
}
