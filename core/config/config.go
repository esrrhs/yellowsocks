package config

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/esrrhs/yellowsocks/core"
	"github.com/esrrhs/yellowsocks/core/sppclient"
	"gopkg.in/yaml.v3"
)

// FileConfig is the YAML or JSON configuration.
type FileConfig struct {
	SPPServer   string            `json:"spp_server" yaml:"spp_server"`
	SPPProto    string            `json:"spp_proto" yaml:"spp_proto"`
	SPPKey      string            `json:"spp_key" yaml:"spp_key"`
	SPPEncrypt  string            `json:"spp_encrypt" yaml:"spp_encrypt"`
	SPPCompress *int              `json:"spp_compress" yaml:"spp_compress"`
	SPPNodes    []*sppclient.Node `json:"nodes" yaml:"nodes"`

	DNSListen   string `json:"dns_listen" yaml:"dns_listen"`
	DoHListen   string `json:"doh_listen" yaml:"doh_listen"`
	DoTListen   string `json:"dot_listen" yaml:"dot_listen"`
	TLSCertFile string `json:"tls_cert" yaml:"tls_cert"`
	TLSKeyFile  string `json:"tls_key" yaml:"tls_key"`
	DirectDNS   string `json:"direct_dns" yaml:"direct_dns"`
	RemoteDoH   string `json:"doh_url" yaml:"doh_url"`

	Socks5Listen  string `json:"socks5_listen" yaml:"socks5_listen"`
	HTTPListen    string `json:"http_listen" yaml:"http_listen"`
	ProxyUsername string `json:"proxy_username" yaml:"proxy_username"`
	ProxyPassword string `json:"proxy_password" yaml:"proxy_password"`

	DirectDomains    []string `json:"direct_domains" yaml:"direct_domains"`
	DirectCIDRs      []string `json:"direct_cidrs" yaml:"direct_cidrs"`
	GeoIPFile        string   `json:"geoip_file" yaml:"geoip_file"`
	ChinaDomainsFile string   `json:"china_domains" yaml:"china_domains"`
	GFWDomainsFile   string   `json:"gfw_domains" yaml:"gfw_domains"`

	LogLevel string `json:"loglevel" yaml:"loglevel"`
}

// LoadConfigFile loads configuration from a JSON or YAML file.
func LoadConfigFile(path string) (*FileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}
	return ParseConfigContent(data)
}

// ParseConfigContent parses YAML or JSON configuration bytes.
func ParseConfigContent(data []byte) (*FileConfig, error) {
	cfg := &FileConfig{}
	if err := json.Unmarshal(data, cfg); err != nil {
		if yamlErr := yaml.Unmarshal(data, cfg); yamlErr != nil {
			return nil, fmt.Errorf("invalid config format (JSON err: %v, YAML err: %v)", err, yamlErr)
		}
	}
	return cfg, nil
}

// MergeWithEngineConfig overlays file settings onto the CLI baseline.
// Values present in the file win.
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
