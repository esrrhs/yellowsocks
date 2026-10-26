package sppclient

import (
	"testing"
	"time"

	"github.com/esrrhs/pingtunnel"
)

func TestPingTunnelClientCloseIdempotent(t *testing.T) {
	p := &pingTunnelClient{closed: true}
	if err := p.Close(); err != nil {
		t.Fatalf("Close on an already closed client: %v", err)
	}
}

func TestPingTunnelClientHasRecentTraffic(t *testing.T) {
	var nilClient *pingTunnelClient
	if nilClient.HasRecentTraffic(time.Minute) {
		t.Fatal("a nil client must report no recent traffic")
	}

	closed := &pingTunnelClient{closed: true}
	if closed.HasRecentTraffic(time.Minute) {
		t.Fatal("a closed client must report no recent traffic")
	}

	// A client whose tunnel never saw a pong (RTT zero) reports no traffic.
	idle := &pingTunnelClient{c: &pingtunnel.Client{}}
	if idle.HasRecentTraffic(time.Minute) {
		t.Fatal("a zero-RTT client must report no recent traffic")
	}
}

func TestParsePingTunnelAuthAndCryptoModes(t *testing.T) {
	// "none" maps to NoEncryption and keeps the default crypto mode.
	if _, c, err := parsePingTunnelAuthAndCrypto("valid-pass-123", "none"); err != nil || c == nil {
		t.Fatalf("none mode: %v", err)
	}
	// The mode may carry a suffix after a colon.
	if _, c, err := parsePingTunnelAuthAndCrypto("valid-pass-123", "aes128:extra"); err != nil || c == nil {
		t.Fatalf("colon mode: %v", err)
	}
	// Whitespace around password and mode is trimmed.
	if _, c, err := parsePingTunnelAuthAndCrypto("  valid-pass-123  ", "  chacha20  "); err != nil || c == nil {
		t.Fatalf("trimmed inputs: %v", err)
	}
}

func TestProbePingTunnelErrorPaths(t *testing.T) {
	// Invalid password fails during parsing.
	if alive, _ := probePingTunnel(&Node{ServerProto: "pingtunnel", Server: "127.0.0.1", Key: "short"}, time.Second); alive {
		t.Fatal("an invalid password must fail the probe")
	}
	// An empty server host fails when creating the throwaway client.
	if alive, _ := probePingTunnel(&Node{ServerProto: "pingtunnel", Server: "", Key: "valid-pass-123"}, time.Second); alive {
		t.Fatal("an empty server must fail the probe")
	}
	// A valid config: without root the ICMP socket cannot be opened, and
	// with root there is no pingtunnel server to answer. Either way the
	// node must not be reported alive.
	if alive, _ := probePingTunnel(&Node{ServerProto: "pingtunnel", Server: "127.0.0.1", Key: "valid-pass-123"}, 300*time.Millisecond); alive {
		t.Fatal("no pong must fail the probe")
	}
}

func TestNewPingTunnelClientListenAddrError(t *testing.T) {
	_, err := newPingTunnelClient(&Config{
		Server:      "127.0.0.1",
		Key:         "valid-pass-123",
		LocalSocks5: "127.0.0.1:99999", // invalid port
	})
	if err == nil {
		t.Fatal("an invalid local socks5 address must fail synchronously")
	}
}

func TestNewPingTunnelClientDefaultAddr(t *testing.T) {
	cl, err := newPingTunnelClient(&Config{Server: "127.0.0.1", Key: "valid-pass-123"})
	if err != nil {
		// Non-root environments cannot open the raw ICMP socket; the
		// failure must be reported synchronously.
		return
	}
	// Running with privileges: the client must be usable and closable.
	if cl.HasRecentTraffic(time.Minute) {
		t.Fatal("a fresh client must report no recent traffic")
	}
	if err := cl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := cl.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
