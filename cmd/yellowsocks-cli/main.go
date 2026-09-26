package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/yellowsocks/core"
	"github.com/esrrhs/yellowsocks/core/config"
	"github.com/esrrhs/yellowsocks/core/version"
)

func main() {
	defer common.CrashLog()

	configPath := flag.String("config", "", "Path to json/yaml configuration file")
	sppServer := flag.String("spp-server", "", "SPP remote server address (e.g. 1.2.3.4:8888)")
	sppProto := flag.String("spp-proto", "tcp", "SPP protocol (tcp, udp, kcp, quic)")
	sppKey := flag.String("spp-key", "123456", "SPP password / key")
	socks5Addr := flag.String("socks5", "127.0.0.1:1080", "Inbound SOCKS5 proxy listen address (TCP+UDP)")
	httpAddr := flag.String("http", "127.0.0.1:8080", "Inbound HTTP/HTTPS proxy listen address")
	dnsAddr := flag.String("dns", "127.0.0.1:53", "Inbound DNS UDP listen address")
	dohAddr := flag.String("doh", "127.0.0.1:8053", "Inbound DNS TCP DoH listen address")
	dotAddr := flag.String("dot", "", "Inbound DNS-over-TLS listen address (e.g. :853)")
	tlsCert := flag.String("tls-cert", "", "TLS certificate PEM for DoT")
	tlsKey := flag.String("tls-key", "", "TLS private key PEM for DoT")
	disableTun := flag.Bool("disable-tun", false, "Disable TUN device (run proxies and DNS only)")
	chinaDomains := flag.String("china-domains", "", "Path to china domain list file")
	gfwDomains := flag.String("gfw-domains", "", "Path to gfw domain list file")
	geoipFile := flag.String("geoip", "", "Path to GeoLite2-Country.mmdb")
	loglevel := flag.String("loglevel", "info", "Log level (debug, info, warn, error)")
	showVersion := flag.Bool("v", false, "Print version information and exit")
	showVersionLong := flag.Bool("version", false, "Print version information and exit")

	flag.Parse()

	if *showVersion || *showVersionLong {
		fmt.Println(version.String())
		return
	}

	var fileCfg *config.FileConfig
	if *configPath != "" {
		var err error
		fileCfg, err = config.LoadConfigFile(*configPath)
		if err != nil {
			fmt.Printf("Error loading config file: %v\n\n", err)
			return
		}
	}

	level := loggo.LEVEL_INFO
	if fileCfg != nil && fileCfg.LogLevel != "" && loggo.NameToLevel(fileCfg.LogLevel) >= 0 {
		level = loggo.NameToLevel(fileCfg.LogLevel)
	} else if loggo.NameToLevel(*loglevel) >= 0 {
		level = loggo.NameToLevel(*loglevel)
	}
	loggo.Ini(loggo.Config{
		Level:  level,
		Prefix: "yellowsocks-cli",
	})

	// Default baseline engine configuration
	cfg := core.EngineConfig{
		TunName:          "tun0",
		TunIP:            "10.255.0.2",
		TunGateway:       "10.255.0.1",
		TunMask:          "255.255.255.0",
		MTU:              1500,
		DisableTun:       *disableTun,
		SPPServer:        *sppServer,
		SPPProto:         *sppProto,
		SPPKey:           *sppKey,
		SPPEncrypt:       "default",
		SPPCompress:      128,
		EnableFakeIP:     true,
		DNSListen:        *dnsAddr,
		DoHListen:        *dohAddr,
		DoTListen:        *dotAddr,
		TLSCertFile:      *tlsCert,
		TLSKeyFile:       *tlsKey,
		DirectDNS:        "1.1.1.1:53",
		RemoteDoH:        "https://1.1.1.1/dns-query",
		SetAutoRoute:     true,
		Socks5Listen:     *socks5Addr,
		HTTPListen:       *httpAddr,
		ChinaDomainsFile: *chinaDomains,
		GFWDomainsFile:   *gfwDomains,
		GeoIPFile:        *geoipFile,
	}

	if fileCfg != nil {
		cfg = fileCfg.MergeWithEngineConfig(cfg)
	}

	if cfg.SPPServer == "" && len(cfg.SPPNodes) == 0 {
		fmt.Println("Usage: yellowsocks-cli -config <config.yaml> (or -spp-server <ip:port> -spp-key <key>)")
		flag.PrintDefaults()
		return
	}

	loggo.Info("Starting YellowSocks CLI Engine (%s)...", version.String())

	engine := core.NewEngine(cfg)
	if err := engine.Start(); err != nil {
		loggo.Error("Failed to start engine: %v", err)
		return
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	loggo.Info("Received signal %v, exiting...", sig)

	_ = engine.Stop()
	loggo.Info("YellowSocks stopped successfully.")
}
