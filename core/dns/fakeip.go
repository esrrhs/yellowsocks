package dns

import (
	"net"

	gohomefakeip "github.com/esrrhs/gohome/dns/fakeip"
)

// FakeIPPool 管理 198.18.0.0/15 保留地址池中的虚拟分配，底层复用 gohome/dns/fakeip
type FakeIPPool struct {
	pool *gohomefakeip.FakeIPPool
}

// NewFakeIPPool 创建 FakeIP 地址池 (RFC 2544 Benchmark 测试保留段: 198.18.0.1 ~ 198.19.255.254)
func NewFakeIPPool() *FakeIPPool {
	return &FakeIPPool{
		pool: gohomefakeip.NewFakeIPPool(),
	}
}

// Allocate 为域名分配或获取已分配的 Fake-IP
func (p *FakeIPPool) Allocate(domain string) net.IP {
	return p.pool.Allocate(domain)
}

// LookupDomainByIP 反查 Fake-IP 对应的真实域名
func (p *FakeIPPool) LookupDomainByIP(ip string) (string, bool) {
	return p.pool.LookupDomainByIPStr(ip)
}

// IsFakeIP 检查 IP 是否属于 Fake-IP 地址段 (198.18.0.0/15)
func IsFakeIP(ip net.IP) bool {
	return gohomefakeip.IsFakeIP(ip)
}

