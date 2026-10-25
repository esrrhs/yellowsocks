package sppclient

import "testing"

func TestIsPingTunnelProto(t *testing.T) {
	for _, p := range []string{"pingtunnel", "PingTunnel", " icmp "} {
		if !isPingTunnelProto(p) {
			t.Fatalf("%q should select pingtunnel", p)
		}
	}
	for _, p := range []string{"", "tcp", "udp", "kcp", "quic", "rudp"} {
		if isPingTunnelProto(p) {
			t.Fatalf("%q should not select pingtunnel", p)
		}
	}
}

func TestPingTunnelHost(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4":          "1.2.3.4",
		"1.2.3.4:0":        "1.2.3.4",
		"pt.example.com":   "pt.example.com",
		"pt.example.com:1": "pt.example.com",
	}
	for in, want := range cases {
		if got := pingTunnelHost(in); got != want {
			t.Fatalf("pingTunnelHost(%q)=%q want %q", in, got, want)
		}
	}
}

func TestParsePingTunnelAuthAndCrypto(t *testing.T) {
	// Password must be at least 8 chars
	for _, short := range []string{"", "1234567", "   "} {
		if _, _, err := parsePingTunnelAuthAndCrypto(short, ""); err == nil {
			t.Fatalf("short password %q should be rejected", short)
		}
	}

	authCode, crypto, err := parsePingTunnelAuthAndCrypto("secret-pass-123", "")
	if err != nil || crypto == nil {
		t.Fatalf("valid password failed: %v", err)
	}
	if authCode == 0 {
		t.Fatal("derived authCode should be non-zero")
	}

	// Encrypt override
	_, cryptoAes, err := parsePingTunnelAuthAndCrypto("secret-pass-123", "aes256")
	if err != nil || cryptoAes == nil {
		t.Fatalf("aes256 override failed: %v", err)
	}

	// Invalid encrypt mode
	if _, _, err := parsePingTunnelAuthAndCrypto("secret-pass-123", "invalid-mode"); err == nil {
		t.Fatal("invalid mode should fail")
	}
}

func TestPingTunnelRobustnessIsLowest(t *testing.T) {
	if protoRobustnessBase("pingtunnel") >= protoRobustnessBase("rudp") {
		t.Fatal("pingtunnel should rank below rudp within a tier")
	}
}

func TestPingTunnelBadConfigFailsSynchronously(t *testing.T) {
	if _, err := newPingTunnelClient(&Config{Server: "127.0.0.1", Key: "short"}); err == nil {
		t.Fatal("short key must fail")
	}
	if _, err := newPingTunnelClient(&Config{}); err == nil {
		t.Fatal("empty server must fail")
	}
}
