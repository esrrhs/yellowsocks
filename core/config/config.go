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

	// IPv6 enables IPv6 destinations through SPP (default false).
	IPv6 *bool `json:"ipv6" yaml:"ipv6"`

	DirectDomains    []string `json:"direct_domains" yaml:"direct_domains"`
	DirectCIDRs      []string `json:"direct_cidrs" yaml:"direct_cidrs"`
	GeoIPFile        string   `json:"geoip_file" yaml:"geoip_file"`
	ChinaDomainsFile string   `json:"china_domains" yaml:"china_domains"`
	GFWDomainsFile   string   `json:"gfw_domains" yaml:"gfw_domains"`

	// Statistics HTTP API
	APIListen string `json:"api_listen" yaml:"api_listen"`
	// APIToken is the admin token. Keep it on the reverse proxy or on the
	// operator's machine, never in a page.
	APIToken string `json:"api_token" yaml:"api_token"`
	// APIReadonlyToken grants GET access only.
	APIReadonlyToken string   `json:"api_readonly_token" yaml:"api_readonly_token"`
	APIOrigins       []string `json:"api_origins" yaml:"api_origins"`
	// APIAllowedCIDRs limits which peers may connect (e.g. only the reverse
	// proxy). Empty means any host with a valid token.
	APIAllowedCIDRs []string `json:"api_allowed_cidrs" yaml:"api_allowed_cidrs"`
	// APIClientCIDRs limits the forwarded client address. Only meaningful
	// together with APITrustedProxies.
	APIClientCIDRs []string `json:"api_client_cidrs" yaml:"api_client_cidrs"`
	// APITrustedProxies lists CIDRs whose X-Forwarded-For is honored.
	APITrustedProxies []string `json:"api_trusted_proxies" yaml:"api_trusted_proxies"`
	APIDashboard      *bool    `json:"api_dashboard" yaml:"api_dashboard"`
	// APIPProf exposes /debug/pprof/ behind the admin token. Off unless set.
	APIPProf *bool `json:"api_pprof" yaml:"api_pprof"`

	// Statistics storage
	StatsFile         string `json:"stats_file" yaml:"stats_file"`
	StatsHistoryHours int    `json:"stats_history_hours" yaml:"stats_history_hours"`

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

	if f.IPv6 != nil {
		res.EnableIPv6 = *f.IPv6
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

	if f.APIListen != "" {
		res.APIListen = f.APIListen
	}
	if f.APIToken != "" {
		res.APIToken = f.APIToken
	}
	if f.APIReadonlyToken != "" {
		res.APIReadonlyToken = f.APIReadonlyToken
	}
	if len(f.APIOrigins) > 0 {
		res.APIOrigins = f.APIOrigins
	}
	if len(f.APIAllowedCIDRs) > 0 {
		res.APIAllowedCIDRs = f.APIAllowedCIDRs
	}
	if len(f.APIClientCIDRs) > 0 {
		res.APIClientCIDRs = f.APIClientCIDRs
	}
	if len(f.APITrustedProxies) > 0 {
		res.APITrustedProxies = f.APITrustedProxies
	}
	if f.APIDashboard != nil {
		res.APIDashboard = *f.APIDashboard
	}
	if f.APIPProf != nil {
		res.APIPProf = *f.APIPProf
	}
	if f.StatsFile != "" {
		res.StatsFile = f.StatsFile
	}
	if f.StatsHistoryHours > 0 {
		res.StatsHistoryHours = f.StatsHistoryHours
	}

	return res
}
