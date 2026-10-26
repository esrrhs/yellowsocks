package sppclient

import (
	"testing"
	"time"
)

func TestNewClientRejectsEmptyServer(t *testing.T) {
	if _, err := NewClient(&Config{}); err == nil {
		t.Fatal("an empty server address must be rejected")
	}
}

// The spp proxy client dials in the background and never fails on a dead
// address, but an unknown transport protocol must fail synchronously.
func TestNewClientInvalidProtoFailsSynchronously(t *testing.T) {
	_, err := NewClient(&Config{
		Server:      "127.0.0.1:1",
		ServerProto: "not-a-real-proto",
		LocalSocks5: reserveTCPPort(t),
	})
	if err == nil {
		t.Fatal("an invalid proto must fail synchronously")
	}
}

func TestNewClientDefaultsAndClose(t *testing.T) {
	cfg := &Config{Server: "127.0.0.1:1", Key: "testkey"} // dead server: spp dials in the background
	cl, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if cfg.ServerProto != "tcp" {
		t.Fatalf("ServerProto=%q want default tcp", cfg.ServerProto)
	}
	if cfg.LocalSocks5 != "127.0.0.1:10808" {
		t.Fatalf("LocalSocks5=%q want default 127.0.0.1:10808", cfg.LocalSocks5)
	}
	if cfg.Name != "yellowsocks_spp" {
		t.Fatalf("Name=%q want default yellowsocks_spp", cfg.Name)
	}

	// Session state is delegated to the underlying proxy client.
	_ = cl.HasRecentTraffic(time.Minute)

	if err := cl.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close is idempotent.
	if err := cl.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if cl.HasRecentTraffic(time.Minute) {
		t.Fatal("a closed client must report no recent traffic")
	}
}

func TestNewClientEncryptDefaultAndKcpFec(t *testing.T) {
	cfg := &Config{
		Server:       "127.0.0.1:1",
		ServerProto:  "kcp",
		Key:          "testkey",
		Encrypt:      "default", // must be translated to "no encryption"
		Compress:     128,
		LocalSocks5:  reserveTCPPort(t),
		KcpFecData:   10,
		KcpFecParity: 3,
	}
	cl, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cl.Close()
	_ = cl.HasRecentTraffic(time.Minute)
}

func TestNilClientHasRecentTraffic(t *testing.T) {
	var c *Client
	if c.HasRecentTraffic(time.Minute) {
		t.Fatal("a nil client must report no recent traffic")
	}
	// A client without a started proxy session reports no traffic either.
	if (&Client{}).HasRecentTraffic(time.Minute) {
		t.Fatal("a client without a proxy must report no recent traffic")
	}
}
