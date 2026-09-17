package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

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
	DirectDNS    string `json:"direct_dns" yaml:"direct_dns"`
	RemoteDoH    string `json:"doh_url" yaml:"doh_url"`
	EnableFakeIP *bool  `json:"fake_ip" yaml:"fake_ip"`

	// Routing & App Bypass Rules
	BypassApps    []string `json:"bypass_apps" yaml:"bypass_apps"`
	DirectDomains []string `json:"direct_domains" yaml:"direct_domains"`
	DirectCIDRs   []string `json:"direct_cidrs" yaml:"direct_cidrs"`

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

	cfg := &FileConfig{}
	if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("invalid YAML config file %s: %w", path, err)
		}
	} else {
		// Default to JSON parser
		if err := json.Unmarshal(data, cfg); err != nil {
			// Fallback try YAML parser if JSON fails
			if yamlErr := yaml.Unmarshal(data, cfg); yamlErr != nil {
				return nil, fmt.Errorf("invalid config file format %s (JSON err: %v, YAML err: %v)", path, err, yamlErr)
			}
		}
	}

	return cfg, nil
}

// MergeWithEngineConfig merges file config with CLI config (CLI flags take precedence)
func (f *FileConfig) MergeWithEngineConfig(base core.EngineConfig, cliSet map[string]bool) core.EngineConfig {
	res := base

	if !cliSet["spp-server"] && f.SPPServer != "" {
		res.SPPServer = f.SPPServer
	}
	if !cliSet["spp-proto"] && f.SPPProto != "" {
		res.SPPProto = f.SPPProto
	}
	if !cliSet["spp-key"] && f.SPPKey != "" {
		res.SPPKey = f.SPPKey
	}
	if !cliSet["spp-encrypt"] && f.SPPEncrypt != "" {
		res.SPPEncrypt = f.SPPEncrypt
	}
	if !cliSet["spp-compress"] && f.SPPCompress != nil {
		res.SPPCompress = *f.SPPCompress
	}
	if len(f.SPPNodes) > 0 {
		res.SPPNodes = f.SPPNodes
	}

	if !cliSet["tun-name"] && f.TunName != "" {
		res.TunName = f.TunName
	}
	if !cliSet["tun-ip"] && f.TunIP != "" {
		res.TunIP = f.TunIP
	}
	if !cliSet["tun-gw"] && f.TunGateway != "" {
		res.TunGateway = f.TunGateway
	}
	if !cliSet["auto-route"] && f.AutoRoute != nil {
		res.SetAutoRoute = *f.AutoRoute
	}

	if !cliSet["dns-listen"] && f.DNSListen != "" {
		res.DNSListen = f.DNSListen
	}
	if !cliSet["direct-dns"] && f.DirectDNS != "" {
		res.DirectDNS = f.DirectDNS
	}
	if !cliSet["doh-url"] && f.RemoteDoH != "" {
		res.RemoteDoH = f.RemoteDoH
	}
	if !cliSet["fake-ip"] && f.EnableFakeIP != nil {
		res.EnableFakeIP = *f.EnableFakeIP
	}

	if !cliSet["bypass-apps"] && len(f.BypassApps) > 0 {
		res.BypassApps = f.BypassApps
	}

	return res
}
