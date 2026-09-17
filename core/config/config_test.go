package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfigContentYAML(t *testing.T) {
	yamlContent := `
spp_server: "1.2.3.4:8888"
spp_proto: "kcp"
spp_key: "secret123"
tun_name: "tun-test"
fake_ip: true
direct_dns: "8.8.8.8:53"
remote_doh: "https://8.8.8.8/dns-query"
bypass_apps:
  - "game.exe"
bypass_cidrs:
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
	if cfg.EnableFakeIP == nil || !*cfg.EnableFakeIP {
		t.Errorf("expected EnableFakeIP true")
	}
	if len(cfg.BypassApps) != 1 || cfg.BypassApps[0] != "game.exe" {
		t.Errorf("unexpected bypass apps: %v", cfg.BypassApps)
	}
}

func TestLoadConfigFileJSON(t *testing.T) {
	jsonContent := `{
		"spp_server": "9.9.9.9:1080",
		"spp_proto": "tcp",
		"spp_key": "pass",
		"auto_route": true
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
	if cfg.AutoRoute == nil || !*cfg.AutoRoute {
		t.Errorf("expected AutoRoute true")
	}
}
