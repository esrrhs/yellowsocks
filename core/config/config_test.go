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

func TestMergeWithEngineConfig(t *testing.T) {
	fakeIP := true
	autoRoute := false
	fileCfg := &FileConfig{
		SPPServer:    "custom.server:9999",
		SPPProto:     "quic",
		SPPKey:       "mykey",
		TunName:      "tun-custom",
		EnableFakeIP: &fakeIP,
		AutoRoute:    &autoRoute,
	}

	base := core.EngineConfig{
		SPPServer:    "default.server:8888",
		SPPProto:     "tcp",
		SPPKey:       "defaultkey",
		TunName:      "tun0",
		SetAutoRoute: true,
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
	if merged.TunName != "tun-custom" {
		t.Errorf("expected TunName tun-custom, got %s", merged.TunName)
	}
	if merged.SetAutoRoute != false {
		t.Errorf("expected SetAutoRoute false, got %v", merged.SetAutoRoute)
	}
	if !merged.EnableFakeIP {
		t.Errorf("expected EnableFakeIP true")
	}
}
