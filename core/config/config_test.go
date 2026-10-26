package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/esrrhs/yellowsocks/core"
	"github.com/esrrhs/yellowsocks/core/sppclient"
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

// Content that is neither valid JSON nor valid YAML for a struct must error
// rather than silently yielding an empty config.
func TestParseConfigContentInvalid(t *testing.T) {
	if _, err := ParseConfigContent([]byte("garbage-not-config")); err == nil {
		t.Fatal("expected error for content that is a scalar in both formats")
	}
	if _, err := ParseConfigContent([]byte("[")); err == nil {
		t.Fatal("expected error for truncated flow sequence")
	}
}

func TestLoadConfigFileMissing(t *testing.T) {
	if _, err := LoadConfigFile(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("expected error for missing config file")
	}
}

// spp_compress and ipv6 are pointers precisely so that explicit zero values
// in the file override nonzero CLI defaults, while an omitted field keeps the
// baseline.
func TestParseConfigPointerZeroValues(t *testing.T) {
	content := `{
		"spp_compress": 0,
		"ipv6": false
	}`
	cfg, err := ParseConfigContent([]byte(content))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.SPPCompress == nil || *cfg.SPPCompress != 0 {
		t.Fatalf("spp_compress should be a non-nil pointer to 0, got %v", cfg.SPPCompress)
	}
	if cfg.IPv6 == nil || *cfg.IPv6 {
		t.Fatalf("ipv6 should be a non-nil pointer to false, got %v", cfg.IPv6)
	}

	base := core.EngineConfig{SPPCompress: 128, EnableIPv6: true}
	merged := cfg.MergeWithEngineConfig(base)
	if merged.SPPCompress != 0 {
		t.Fatalf("explicit compress=0 must override baseline, got %d", merged.SPPCompress)
	}
	if merged.EnableIPv6 {
		t.Fatal("explicit ipv6=false must override baseline true")
	}
}

// An empty file config leaves every CLI baseline value untouched.
func TestMergeEmptyConfigKeepsBaseline(t *testing.T) {
	base := core.EngineConfig{
		SPPServer:    "base:8888",
		SPPProto:     "tcp",
		SPPKey:       "basekey",
		SPPCompress:  128,
		EnableIPv6:   true,
		DNSListen:    "127.0.0.1:53",
		DirectDNS:    "1.1.1.1:53",
		RemoteDoH:    "https://1.1.1.1/dns-query",
		Socks5Listen: "127.0.0.1:1080",
	}
	merged := (&FileConfig{}).MergeWithEngineConfig(base)
	if !reflect.DeepEqual(merged, base) {
		t.Fatalf("empty file config changed baseline:\nbase=%+v\ngot =%+v", base, merged)
	}
}

// Every field set in the file config must win over the CLI baseline, and
// baseline-only fields that the file cannot express must survive the merge.
func TestMergeWithEngineConfigFullFileOverrides(t *testing.T) {
	compress := 64
	ipv6 := true
	dashboard := true
	pprof := false
	nodes := []*sppclient.Node{
		{Name: "alpha", Server: "5.6.7.8:4430", ServerProto: "quic", Key: "k1", Priority: 5, Bandwidth: 200},
	}
	fileCfg := &FileConfig{
		SPPServer:   "file.server:1234",
		SPPProto:    "kcp",
		SPPKey:      "filekey",
		SPPEncrypt:  "aes",
		SPPCompress: &compress,
		SPPNodes:    nodes,

		DNSListen:   "127.0.0.1:5353",
		DoHListen:   "127.0.0.1:8053",
		DoTListen:   "127.0.0.1:8853",
		TLSCertFile: "/tls/cert.pem",
		TLSKeyFile:  "/tls/key.pem",
		DirectDNS:   "9.9.9.9:53",
		RemoteDoH:   "https://9.9.9.9/dns-query",

		Socks5Listen:  "127.0.0.1:2080",
		HTTPListen:    "127.0.0.1:8180",
		ProxyUsername: "user",
		ProxyPassword: "pass",

		IPv6: &ipv6,

		DirectDomains:    []string{"example.com"},
		DirectCIDRs:      []string{"192.168.0.0/16"},
		GeoIPFile:        "/geo/GeoLite2-Country.mmdb",
		ChinaDomainsFile: "/lists/china.txt",
		GFWDomainsFile:   "/lists/gfw.txt",

		APIListen:        "127.0.0.1:9090",
		APIToken:         "admin-token",
		APIReadonlyToken: "ro-token",
		APIOrigins:       []string{"https://panel.example.com"},

		APIAllowedCIDRs:   []string{"127.0.0.0/8"},
		APIClientCIDRs:    []string{"10.0.0.0/8"},
		APITrustedProxies: []string{"172.16.0.0/12"},
		APIDashboard:      &dashboard,
		APIPProf:          &pprof,

		StatsFile:         "/var/lib/yellowsocks/stats.gob",
		StatsHistoryHours: 72,
	}

	base := core.EngineConfig{
		SPPServer:         "base:1",
		SPPProto:          "tcp",
		SPPKey:            "basekey",
		SPPEncrypt:        "none",
		SPPCompress:       128,
		LocalSocks5:       "127.0.0.1:1081",
		DNSListen:         "base-dns:53",
		DoHListen:         "base-doh:8053",
		DoTListen:         "base-dot:8853",
		TLSCertFile:       "base-cert",
		TLSKeyFile:        "base-key",
		DirectDNS:         "base-ddns:53",
		RemoteDoH:         "https://base/dns-query",
		Socks5Listen:      "base:1080",
		HTTPListen:        "base:8180",
		ProxyUsername:     "baseuser",
		ProxyPassword:     "basepass",
		EnableIPv6:        false,
		DirectDomains:     []string{"base.example"},
		DirectCIDRs:       []string{"10.0.0.0/8"},
		GeoIPFile:         "base.mmdb",
		ChinaDomainsFile:  "base-china.txt",
		GFWDomainsFile:    "base-gfw.txt",
		APIListen:         "base-api:9090",
		APIToken:          "base-admin",
		APIReadonlyToken:  "base-ro",
		APIOrigins:        []string{"https://base.example"},
		APIAllowedCIDRs:   []string{"0.0.0.0/0"},
		APIClientCIDRs:    []string{"0.0.0.0/0"},
		APITrustedProxies: []string{"0.0.0.0/0"},
		APIDashboard:      false,
		APIPProf:          true,
		StatsFile:         "base-stats.gob",
		StatsHistoryHours: 24,
	}

	merged := fileCfg.MergeWithEngineConfig(base)

	if merged.SPPServer != "file.server:1234" {
		t.Errorf("SPPServer=%q", merged.SPPServer)
	}
	if merged.SPPProto != "kcp" {
		t.Errorf("SPPProto=%q", merged.SPPProto)
	}
	if merged.SPPKey != "filekey" {
		t.Errorf("SPPKey=%q", merged.SPPKey)
	}
	if merged.SPPEncrypt != "aes" {
		t.Errorf("SPPEncrypt=%q", merged.SPPEncrypt)
	}
	if merged.SPPCompress != 64 {
		t.Errorf("SPPCompress=%d", merged.SPPCompress)
	}
	if !reflect.DeepEqual(merged.SPPNodes, nodes) {
		t.Errorf("SPPNodes=%+v", merged.SPPNodes)
	}
	if merged.LocalSocks5 != "127.0.0.1:1081" {
		t.Errorf("baseline-only LocalSocks5 must survive, got %q", merged.LocalSocks5)
	}

	if merged.DNSListen != "127.0.0.1:5353" || merged.DoHListen != "127.0.0.1:8053" || merged.DoTListen != "127.0.0.1:8853" {
		t.Errorf("dns listeners=%q %q %q", merged.DNSListen, merged.DoHListen, merged.DoTListen)
	}
	if merged.TLSCertFile != "/tls/cert.pem" || merged.TLSKeyFile != "/tls/key.pem" {
		t.Errorf("tls files=%q %q", merged.TLSCertFile, merged.TLSKeyFile)
	}
	if merged.DirectDNS != "9.9.9.9:53" || merged.RemoteDoH != "https://9.9.9.9/dns-query" {
		t.Errorf("dns upstream=%q %q", merged.DirectDNS, merged.RemoteDoH)
	}

	if merged.Socks5Listen != "127.0.0.1:2080" || merged.HTTPListen != "127.0.0.1:8180" {
		t.Errorf("proxy listens=%q %q", merged.Socks5Listen, merged.HTTPListen)
	}
	if merged.ProxyUsername != "user" || merged.ProxyPassword != "pass" {
		t.Errorf("proxy auth=%q %q", merged.ProxyUsername, merged.ProxyPassword)
	}
	if !merged.EnableIPv6 {
		t.Error("explicit ipv6=true must override baseline false")
	}

	if !reflect.DeepEqual(merged.DirectDomains, []string{"example.com"}) {
		t.Errorf("DirectDomains=%v", merged.DirectDomains)
	}
	if !reflect.DeepEqual(merged.DirectCIDRs, []string{"192.168.0.0/16"}) {
		t.Errorf("DirectCIDRs=%v", merged.DirectCIDRs)
	}
	if merged.GeoIPFile != "/geo/GeoLite2-Country.mmdb" || merged.ChinaDomainsFile != "/lists/china.txt" || merged.GFWDomainsFile != "/lists/gfw.txt" {
		t.Errorf("routing files=%q %q %q", merged.GeoIPFile, merged.ChinaDomainsFile, merged.GFWDomainsFile)
	}

	if merged.APIListen != "127.0.0.1:9090" {
		t.Errorf("APIListen=%q", merged.APIListen)
	}
	if merged.APIToken != "admin-token" || merged.APIReadonlyToken != "ro-token" {
		t.Errorf("api tokens=%q %q", merged.APIToken, merged.APIReadonlyToken)
	}
	if !reflect.DeepEqual(merged.APIOrigins, []string{"https://panel.example.com"}) {
		t.Errorf("APIOrigins=%v", merged.APIOrigins)
	}
	if !reflect.DeepEqual(merged.APIAllowedCIDRs, []string{"127.0.0.0/8"}) {
		t.Errorf("APIAllowedCIDRs=%v", merged.APIAllowedCIDRs)
	}
	if !reflect.DeepEqual(merged.APIClientCIDRs, []string{"10.0.0.0/8"}) {
		t.Errorf("APIClientCIDRs=%v", merged.APIClientCIDRs)
	}
	if !reflect.DeepEqual(merged.APITrustedProxies, []string{"172.16.0.0/12"}) {
		t.Errorf("APITrustedProxies=%v", merged.APITrustedProxies)
	}
	if !merged.APIDashboard {
		t.Error("explicit api_dashboard=true must override baseline false")
	}
	if merged.APIPProf {
		t.Error("explicit api_pprof=false must override baseline true")
	}

	if merged.StatsFile != "/var/lib/yellowsocks/stats.gob" {
		t.Errorf("StatsFile=%q", merged.StatsFile)
	}
	if merged.StatsHistoryHours != 72 {
		t.Errorf("StatsHistoryHours=%d", merged.StatsHistoryHours)
	}

	want := base
	want.SPPServer = "file.server:1234"
	want.SPPProto = "kcp"
	want.SPPKey = "filekey"
	want.SPPEncrypt = "aes"
	want.SPPCompress = 64
	want.SPPNodes = nodes
	want.DNSListen = "127.0.0.1:5353"
	want.DoHListen = "127.0.0.1:8053"
	want.DoTListen = "127.0.0.1:8853"
	want.TLSCertFile = "/tls/cert.pem"
	want.TLSKeyFile = "/tls/key.pem"
	want.DirectDNS = "9.9.9.9:53"
	want.RemoteDoH = "https://9.9.9.9/dns-query"
	want.Socks5Listen = "127.0.0.1:2080"
	want.HTTPListen = "127.0.0.1:8180"
	want.ProxyUsername = "user"
	want.ProxyPassword = "pass"
	want.EnableIPv6 = true
	want.DirectDomains = []string{"example.com"}
	want.DirectCIDRs = []string{"192.168.0.0/16"}
	want.GeoIPFile = "/geo/GeoLite2-Country.mmdb"
	want.ChinaDomainsFile = "/lists/china.txt"
	want.GFWDomainsFile = "/lists/gfw.txt"
	want.APIListen = "127.0.0.1:9090"
	want.APIToken = "admin-token"
	want.APIReadonlyToken = "ro-token"
	want.APIOrigins = []string{"https://panel.example.com"}
	want.APIAllowedCIDRs = []string{"127.0.0.0/8"}
	want.APIClientCIDRs = []string{"10.0.0.0/8"}
	want.APITrustedProxies = []string{"172.16.0.0/12"}
	want.APIDashboard = true
	want.APIPProf = false
	want.StatsFile = "/var/lib/yellowsocks/stats.gob"
	want.StatsHistoryHours = 72
	if !reflect.DeepEqual(merged, want) {
		t.Fatalf("merged config mismatch:\nwant=%+v\ngot =%+v", want, merged)
	}
}

// The nodes list parses from YAML into sppclient.Node entries with all
// per-node attributes intact.
func TestParseConfigContentSPPNodes(t *testing.T) {
	content := `
nodes:
  - name: "alpha"
    server: "5.6.7.8:4430"
    server_proto: "quic"
    key: "k1"
    encrypt: "aes"
    compress: 1
    priority: 5
    bandwidth: 200
    kcpfecdata: 10
    kcpfecparity: 3
  - name: "beta"
    server: "5.6.7.9:4431"
`
	cfg, err := ParseConfigContent([]byte(content))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.SPPNodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(cfg.SPPNodes))
	}
	n := cfg.SPPNodes[0]
	if n.Name != "alpha" || n.Server != "5.6.7.8:4430" || n.ServerProto != "quic" || n.Key != "k1" {
		t.Fatalf("node 0 fields: %+v", n)
	}
	if n.Encrypt != "aes" || n.Compress != 1 || n.Priority != 5 || n.Bandwidth != 200 {
		t.Fatalf("node 0 tuning fields: %+v", n)
	}
	if n.KcpFecData != 10 || n.KcpFecParity != 3 {
		t.Fatalf("node 0 kcp fec: %+v", n)
	}
	if cfg.SPPNodes[1].Name != "beta" || cfg.SPPNodes[1].Server != "5.6.7.9:4431" {
		t.Fatalf("node 1 fields: %+v", cfg.SPPNodes[1])
	}
}
