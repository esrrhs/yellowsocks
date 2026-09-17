package dns

import (
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
)

// FakeIPPool 管理 198.18.0.0/15 保留地址池中的虚拟分配
type FakeIPPool struct {
	baseIP     uint32
	maxOffset  uint32
	current    uint32
	domainToIP sync.Map // domain -> fakeIP (string)
	ipToDomain sync.Map // fakeIP (string) -> domain
}

// NewFakeIPPool 创建 FakeIP 地址池 (RFC 2544 Benchmark 测试保留段: 198.18.0.1 ~ 198.19.255.254)
func NewFakeIPPool() *FakeIPPool {
	// 198.18.0.1
	base := (uint32(198) << 24) | (uint32(18) << 16) | (uint32(0) << 8) | uint32(1)
	// 65534 个地址 (也可扩展到 /15 即 131070)
	maxOffset := uint32(65530)

	return &FakeIPPool{
		baseIP:    base,
		maxOffset: maxOffset,
		current:   1,
	}
}

// Allocate 为域名分配或获取已分配的 Fake-IP
func (p *FakeIPPool) Allocate(domain string) net.IP {
	if val, ok := p.domainToIP.Load(domain); ok {
		return val.(net.IP)
	}

	offset := atomic.AddUint32(&p.current, 1) % p.maxOffset
	ipNum := p.baseIP + offset

	ipBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(ipBytes, ipNum)
	ip := net.IP(ipBytes)

	p.domainToIP.Store(domain, ip)
	p.ipToDomain.Store(ip.String(), domain)

	return ip
}

// LookupDomainByIP 反查 Fake-IP 对应的真实域名
func (p *FakeIPPool) LookupDomainByIP(ip string) (string, bool) {
	val, ok := p.ipToDomain.Load(ip)
	if !ok {
		return "", false
	}
	return val.(string), true
}

// IsFakeIP 检查 IP 是否属于 Fake-IP 地址段 (198.18.0.0/15)
func IsFakeIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	return ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19)
}
