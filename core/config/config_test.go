package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/esrrhs/yellowsocks/core"
)

func TestParseConfigContentYAML(t *testing.T) {
	yamlContent := `
spp_server: "1.2.3.4:8888"
spp_proto: "kcp"
spp_key: "secret123"
direct_dns: "8.8.8.8:53"
doh_url: "https://8.8.8.8/dns-query"
socks5_listen: "127.0.0.1:1080"
http_listen: "127.0.0.1:8080"
direct_cidrs:
  - "10.0.0.0/8"
`
	cfg, err := ParseConfigContent([]byte(yamlContent))
	if err != nil {
		t.Fatalf("Failed to parse YAML config: %v", err)
	}

	if cfg.SPPServer != "1.2.3.4:8888" {
		t.Errorf("expected server 1.2.3.4:8888, got %s", cfg.SPPServer)
	}
	if cfg.SPPProto != "kcp" {
		t.Errorf("expected proto kcp, got %s", cfg.SPPProto)
	}
	if cfg.SPPKey != "secret123" {
		t.Errorf("expected key secret123, got %s", cfg.SPPKey)
	}
	if cfg.DirectDNS != "8.8.8.8:53" {
		t.Errorf("expected direct dns, got %s", cfg.DirectDNS)
	}
	if cfg.RemoteDoH != "https://8.8.8.8/dns-query" {
		t.Errorf("expected doh url, got %s", cfg.RemoteDoH)
	}
	if len(cfg.DirectCIDRs) != 1 || cfg.DirectCIDRs[0] != "10.0.0.0/8" {
		t.Errorf("unexpected direct cidrs: %v", cfg.DirectCIDRs)
	}
}

func TestLoadConfigFileJSON(t *testing.T) {
	jsonContent := `{
		"spp_server": "9.9.9.9:1080",
		"spp_proto": "tcp",
		"spp_key": "pass",
		"dot_listen": ":853"
	}`
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "config.json")
	if err := os.WriteFile(filePath, []byte(jsonContent), 0644); err != nil {
		t.Fatalf("failed to write temp config file: %v", err)
	}

	cfg, err := LoadConfigFile(filePath)
	if err != nil {
		t.Fatalf("failed to load JSON config file: %v", err)
	}

	if cfg.SPPServer != "9.9.9.9:1080" {
		t.Errorf("expected 9.9.9.9:1080, got %s", cfg.SPPServer)
	}
	if cfg.DoTListen != ":853" {
		t.Errorf("expected DoT listen :853, got %s", cfg.DoTListen)
	}
}

func TestMergeWithEngineConfig(t *testing.T) {
	compress := 64
	fileCfg := &FileConfig{
		SPPServer:    "custom.server:9999",
		SPPProto:     "quic",
		SPPKey:       "mykey",
		SPPCompress:  &compress,
		DoTListen:    ":853",
		TLSCertFile:  "/certs/fullchain.pem",
		TLSKeyFile:   "/certs/privkey.pem",
		Socks5Listen: "127.0.0.1:1080",
		HTTPListen:   "127.0.0.1:8080",
	}

	base := core.EngineConfig{
		SPPServer: "default.server:8888",
		SPPProto:  "tcp",
		SPPKey:    "defaultkey",
	}

	merged := fileCfg.MergeWithEngineConfig(base)

	if merged.SPPServer != "custom.server:9999" {
		t.Errorf("expected SPPServer custom.server:9999, got %s", merged.SPPServer)
	}
	if merged.SPPProto != "quic" {
		t.Errorf("expected SPPProto quic, got %s", merged.SPPProto)
	}
	if merged.SPPKey != "mykey" {
		t.Errorf("expected SPPKey mykey, got %s", merged.SPPKey)
	}
	if merged.SPPCompress != 64 {
		t.Errorf("expected SPPCompress 64, got %d", merged.SPPCompress)
	}
	if merged.DoTListen != ":853" {
		t.Errorf("expected DoTListen :853, got %s", merged.DoTListen)
	}
	if merged.TLSCertFile != "/certs/fullchain.pem" || merged.TLSKeyFile != "/certs/privkey.pem" {
		t.Errorf("unexpected TLS paths: %s %s", merged.TLSCertFile, merged.TLSKeyFile)
	}
	if merged.Socks5Listen != "127.0.0.1:1080" || merged.HTTPListen != "127.0.0.1:8080" {
		t.Errorf("unexpected proxy listen: %s %s", merged.Socks5Listen, merged.HTTPListen)
	}
}

func TestParseConfigDoTFields(t *testing.T) {
	yamlContent := `
dot_listen: ":853"
tls_cert: "/path/cert.pem"
tls_key: "/path/key.pem"
dns_listen: ":53"
`
	cfg, err := ParseConfigContent([]byte(yamlContent))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.DoTListen != ":853" || cfg.TLSCertFile != "/path/cert.pem" || cfg.TLSKeyFile != "/path/key.pem" {
		t.Fatalf("unexpected DoT fields: %+v", cfg)
	}
	if cfg.DNSListen != ":53" {
		t.Fatalf("dns_listen=%q", cfg.DNSListen)
	}
}
