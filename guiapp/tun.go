package guiapp

import "time"

// TunParams describes the engine endpoints the TUN stack forwards into.
// The engine keeps making every routing decision (China direct vs proxy)
// and keeps the statistics; the TUN layer only feeds system traffic into
// the engine's existing SOCKS5 inbound and hijacks port 53 DNS.
type TunParams struct {
	// Socks5Addr is the engine's inbound SOCKS5 listen address (TCP CONNECT
	// and UDP ASSOCIATE), e.g. "127.0.0.1:10808".
	Socks5Addr string
	// DNSAddr is the engine's DNS listener (e.g. "127.0.0.1:1053"); port-53
	// datagrams captured on the TUN are forwarded here raw, so the smart
	// China/foreign DNS split applies system-wide.
	DNSAddr string
	// UpstreamIPs are the upstream server IPs; they get /32 routes through
	// the physical gateway so the proxy's own tunnel never loops into the
	// TUN (the SPP library dials from inside its own code, where no socket
	// option can be injected).
	UpstreamIPs []string
	// OnRollback is called when the watchdog tears the TUN down because the
	// forwarding pipeline failed its self-probe. It receives the reason.
	OnRollback func(reason string)
	// Soak extends the watchdog's self-probe window (default: two probes
	// 3s apart). Diagnostics runs set a longer soak so the live adapter can
	// be inspected before any rollback.
	Soak time.Duration
}

// tunWatchdogSoak overrides the watchdog soak window for all subsequent
// enables (diagnostics hook, see SetTunWatchdogSoak).
var tunWatchdogSoak time.Duration

// SetTunWatchdogSoak overrides the watchdog soak window.
func SetTunWatchdogSoak(d time.Duration) { tunWatchdogSoak = d }

// Tun constants shared by the platform implementations and the GUI wiring.
const (
	tunAdapterName = "YellowSocks"
	tunIPv4        = "198.18.0.1"        // adapter address (host route /32)
	tunDNSv4       = "198.18.0.2"        // gateway peer (route nexthop)
	tunProbeDNS    = "223.5.5.5"         // adapter DNS (AliDNS, reachable via the pipeline)
	tunGatewayMAC4 = "02-50-59-31-38-32" // static neighbor MAC for the gateway
	tunMTU         = 1500
)
