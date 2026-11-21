package proxy

// StaticUpstream is an UpstreamProvider that points at a single, fixed
// SOCKS5 server. It lets the engine forward proxied traffic to any generic
// RFC 1928 SOCKS5 server (supporting TCP CONNECT and optionally UDP
// ASSOCIATE) without going through the SPP manager.
type StaticUpstream struct {
	addr string
	user string
	pass string
}

// NewStaticUpstream creates a static upstream provider. user/pass may be
// empty when the server needs no authentication.
func NewStaticUpstream(addr, user, pass string) *StaticUpstream {
	return &StaticUpstream{addr: addr, user: user, pass: pass}
}

// Socks5Addr returns the fixed upstream address.
func (u *StaticUpstream) Socks5Addr() string { return u.addr }

// Socks5Auth returns the configured upstream credentials.
func (u *StaticUpstream) Socks5Auth() (string, string) { return u.user, u.pass }

// Addr returns the upstream address (used for display in the API).
func (u *StaticUpstream) Addr() string { return u.addr }
