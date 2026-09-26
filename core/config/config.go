package config

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/esrrhs/yellowsocks/core"
	"github.com/esrrhs/yellowsocks/core/sppclient"
	"gopkg.in/yaml.v3"
)

// FileConfig defines configuration structure for JSON and YAML files
type FileConfig struct {
	// SPP Upstream Settings
	SPPServer   string            `json:"spp_server" yaml:"spp_server"`
	SPPProto    string            `json:"spp_proto" yaml:"spp_proto"`
	SPPKey      string            `json:"spp_key" yaml:"spp_key"`
	SPPEncrypt  string            `json:"spp_encrypt" yaml:"spp_encrypt"`
	SPPCompress *int              `json:"spp_compress" yaml:"spp_compress"`
	SPPNodes    []*sppclient.Node `json:"nodes" yaml:"nodes"`

	// Virtual TUN Device Settings
	TunName    string `json:"tun_name" yaml:"tun_name"`
	TunIP      string `json:"tun_ip" yaml:"tun_ip"`
	TunGateway string `json:"tun_gw" yaml:"tun_gw"`
	AutoRoute  *bool  `json:"auto_route" yaml:"auto_route"`

	// DNS & Fake-IP Settings
	DNSListen    string `json:"dns_listen" yaml:"dns_listen"`
	DoHListen    string `json:"doh_listen" yaml:"doh_listen"`
	DoTListen    string `json:"dot_listen" yaml:"dot_listen"`
	TLSCertFile  string `json:"tls_cert" yaml:"tls_cert"`
	TLSKeyFile   string `json:"tls_key" yaml:"tls_key"`
	DirectDNS    string `json:"direct_dns" yaml:"direct_dns"`
	RemoteDoH    string `json:"doh_url" yaml:"doh_url"`
	EnableFakeIP *bool  `json:"fake_ip" yaml:"fake_ip"`

	// Inbound Proxy Settings
	Socks5Listen  string `json:"socks5_listen" yaml:"socks5_listen"`
	HTTPListen    string `json:"http_listen" yaml:"http_listen"`
	ProxyUsername string `json:"proxy_username" yaml:"proxy_username"`
	ProxyPassword string `json:"proxy_password" yaml:"proxy_password"`
	DisableTun    *bool  `json:"disable_tun" yaml:"disable_tun"`

	// Routing & App Bypass Rules
	BypassApps       []string `json:"bypass_apps" yaml:"bypass_apps"`
	DirectDomains    []string `json:"direct_domains" yaml:"direct_domains"`
	DirectCIDRs      []string `json:"direct_cidrs" yaml:"direct_cidrs"`
	GeoIPFile        string   `json:"geoip_file" yaml:"geoip_file"`
	ChinaDomainsFile string   `json:"china_domains" yaml:"china_domains"`
	GFWDomainsFile   string   `json:"gfw_domains" yaml:"gfw_domains"`

	// General
	LogLevel string `json:"loglevel" yaml:"loglevel"`
	WebPort  *int   `json:"web_port" yaml:"web_port"`
}

// LoadConfigFile loads configuration from a JSON or YAML file
func LoadConfigFile(path string) (*FileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}
	return ParseConfigContent(data)
}

// ParseConfigContent parses configuration from byte slice (YAML or JSON)
func ParseConfigContent(data []byte) (*FileConfig, error) {
	cfg := &FileConfig{}
	// Try JSON first
	if err := json.Unmarshal(data, cfg); err != nil {
		// Fallback to YAML
		if yamlErr := yaml.Unmarshal(data, cfg); yamlErr != nil {
			return nil, fmt.Errorf("invalid config format (JSON err: %v, YAML err: %v)", err, yamlErr)
		}
	}
	return cfg, nil
}

// MergeWithEngineConfig merges file config with base EngineConfig.
// When configuration file specifies a parameter, it takes absolute precedence over CLI flags.
func (f *FileConfig) MergeWithEngineConfig(base core.EngineConfig) core.EngineConfig {
	res := base

	if f.SPPServer != "" {
		res.SPPServer = f.SPPServer
	}
	if f.SPPProto != "" {
		res.SPPProto = f.SPPProto
	}
	if f.SPPKey != "" {
		res.SPPKey = f.SPPKey
	}
	if f.SPPEncrypt != "" {
		res.SPPEncrypt = f.SPPEncrypt
	}
	if f.SPPCompress != nil {
		res.SPPCompress = *f.SPPCompress
	}
	if len(f.SPPNodes) > 0 {
		res.SPPNodes = f.SPPNodes
	}

	if f.TunName != "" {
		res.TunName = f.TunName
	}
	if f.TunIP != "" {
		res.TunIP = f.TunIP
	}
	if f.TunGateway != "" {
		res.TunGateway = f.TunGateway
	}
	if f.AutoRoute != nil {
		res.SetAutoRoute = *f.AutoRoute
	}

	if f.DNSListen != "" {
		res.DNSListen = f.DNSListen
	}
	if f.DoHListen != "" {
		res.DoHListen = f.DoHListen
	}
	if f.DoTListen != "" {
		res.DoTListen = f.DoTListen
	}
	if f.TLSCertFile != "" {
		res.TLSCertFile = f.TLSCertFile
	}
	if f.TLSKeyFile != "" {
		res.TLSKeyFile = f.TLSKeyFile
	}
	if f.DirectDNS != "" {
		res.DirectDNS = f.DirectDNS
	}
	if f.RemoteDoH != "" {
		res.RemoteDoH = f.RemoteDoH
	}
	if f.EnableFakeIP != nil {
		res.EnableFakeIP = *f.EnableFakeIP
	}

	if f.Socks5Listen != "" {
		res.Socks5Listen = f.Socks5Listen
	}
	if f.HTTPListen != "" {
		res.HTTPListen = f.HTTPListen
	}
	if f.ProxyUsername != "" {
		res.ProxyUsername = f.ProxyUsername
	}
	if f.ProxyPassword != "" {
		res.ProxyPassword = f.ProxyPassword
	}
	if f.DisableTun != nil {
		res.DisableTun = *f.DisableTun
	}

	if len(f.BypassApps) > 0 {
		res.BypassApps = f.BypassApps
	}
	if len(f.DirectDomains) > 0 {
		res.DirectDomains = f.DirectDomains
	}
	if len(f.DirectCIDRs) > 0 {
		res.DirectCIDRs = f.DirectCIDRs
	}
	if f.GeoIPFile != "" {
		res.GeoIPFile = f.GeoIPFile
	}
	if f.ChinaDomainsFile != "" {
		res.ChinaDomainsFile = f.ChinaDomainsFile
	}
	if f.GFWDomainsFile != "" {
		res.GFWDomainsFile = f.GFWDomainsFile
	}

	return res
}
