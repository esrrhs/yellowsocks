package sppclient

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/pingtunnel"
)

// ProtoPingTunnel marks a node as a pingtunnel (ICMP tunnel) upstream.
// For such nodes:
//   - Server is the pingtunnel server host (a ":port" suffix is ignored, ICMP has no ports).
//   - Key is the shared password (min 8 characters).
//   - Encrypt is the encryption algorithm override: "chacha20" (default), "aes256", or "aes128".
//
// The client needs permission to open a raw ICMP socket (root on Linux/macOS).
const ProtoPingTunnel = "pingtunnel"

// isPingTunnelProto reports whether proto selects the pingtunnel upstream.
func isPingTunnelProto(proto string) bool {
	switch strings.ToLower(strings.TrimSpace(proto)) {
	case ProtoPingTunnel, "icmp":
		return true
	}
	return false
}

// pingTunnelHost strips an optional port from a pingtunnel server address.
func pingTunnelHost(server string) string {
	server = strings.TrimSpace(server)
	if host, _, err := net.SplitHostPort(server); err == nil && host != "" {
		return host
	}
	return server
}

// parsePingTunnelAuthAndCrypto parses the node's key (treated as pingtunnel v3 password)
// and optional encrypt override (chacha20, aes128, aes256), returning the authCode and CryptoConfig.
func parsePingTunnelAuthAndCrypto(password string, encrypt string) (int, *pingtunnel.CryptoConfig, error) {
	password = strings.TrimSpace(password)
	if err := pingtunnel.ValidatePassword(password); err != nil {
		return 0, nil, fmt.Errorf("pingtunnel password invalid: %w", err)
	}
	authCode := pingtunnel.DeriveAuthCode(password)

	encMode := pingtunnel.CHACHA20
	encrypt = strings.TrimSpace(encrypt)
	if encrypt != "" && !strings.EqualFold(encrypt, "default") {
		modeStr := encrypt
		if m, _, ok := strings.Cut(encrypt, ":"); ok {
			modeStr = m
		}
		mode, err := pingtunnel.ParseEncryptionMode(strings.TrimSpace(modeStr))
		if err != nil {
			return 0, nil, err
		}
		if mode != pingtunnel.NoEncryption {
			encMode = mode
		}
	}

	crypto, err := pingtunnel.NewCryptoConfig(encMode, password)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to create pingtunnel crypto: %w", err)
	}
	return authCode, crypto, nil
}

// pingTunnelClient wraps pingtunnel.Client and exposes a local SOCKS5 listener,
// mirroring what Client does for SPP.
type pingTunnelClient struct {
	c      *pingtunnel.Client
	mu     sync.Mutex
	closed bool
}

// newPingTunnelClient starts a pingtunnel client in SOCKS5 mode listening on
// cfg.LocalSocks5. It fails synchronously when the ICMP socket or the local
// listener cannot be opened (typically: not running as root).
func newPingTunnelClient(cfg *Config) (*pingTunnelClient, error) {
	if strings.TrimSpace(cfg.Server) == "" {
		return nil, fmt.Errorf("pingtunnel server address cannot be empty")
	}
	if cfg.LocalSocks5 == "" {
		cfg.LocalSocks5 = "127.0.0.1:10808"
	}
	authCode, crypto, err := parsePingTunnelAuthAndCrypto(cfg.Key, cfg.Encrypt)
	if err != nil {
		return nil, err
	}

	loggo.Info("[PingTunnel] Starting client to %s, local socks5: %s", cfg.Server, cfg.LocalSocks5)
	// SOCKS5 mode runs with udpMode=false, socks5=true.
	c, err := pingtunnel.NewClient(cfg.LocalSocks5, pingTunnelHost(cfg.Server), "", 60, authCode, "0.0.0.0",
		false, true, 0, crypto,
		"", "", "bb")
	if err != nil {
		return nil, fmt.Errorf("failed to init pingtunnel client: %w", err)
	}
	if err := c.Run(); err != nil {
		return nil, fmt.Errorf("failed to start pingtunnel client (raw ICMP needs root): %w", err)
	}
	return &pingTunnelClient{c: c}, nil
}

// Close stops the pingtunnel client and releases its listeners.
func (p *pingTunnelClient) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	p.c.Stop()
	return nil
}

// HasRecentTraffic reports whether the tunnel currently has a live session.
// pingtunnel zeroes its RTT when no pong arrived for 3 seconds, so a positive
// RTT means the server answered recently.
func (p *pingTunnelClient) HasRecentTraffic(window time.Duration) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	return !closed && p.c.RTT() > 0
}

// probePingTunnel checks reachability by running a throwaway pingtunnel client
// and waiting for the server's pong. A plain ICMP echo cannot be used: the
// pingtunnel server deliberately has the kernel's echo reply disabled.
func probePingTunnel(node *Node, timeout time.Duration) (bool, time.Duration) {
	authCode, crypto, err := parsePingTunnelAuthAndCrypto(node.Key, node.Encrypt)
	if err != nil {
		return false, 0
	}
	// UDP mode on an ephemeral loopback port: we only need the ping/pong exchange.
	c, err := pingtunnel.NewClient("127.0.0.1:0", pingTunnelHost(node.Server), "", 60, authCode, "0.0.0.0",
		true, false, 0, crypto,
		"", "", "")
	if err != nil {
		return false, 0
	}
	if err := c.Run(); err != nil {
		return false, 0
	}
	defer c.Stop()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rtt := c.RTT(); rtt > 0 {
			return true, rtt
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false, 0
}
