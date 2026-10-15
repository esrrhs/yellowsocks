package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/yellowsocks/core"
	"github.com/esrrhs/yellowsocks/core/config"
	"github.com/esrrhs/yellowsocks/core/launch"
	"github.com/esrrhs/yellowsocks/core/version"
)

func main() {
	defer common.CrashLog()
	os.Exit(run(os.Args[1:], os.Stdout))
}

// run parses arguments, builds and starts the engine, then waits for a
// termination signal. It returns a process exit code and writes all
// user-facing output to stdout, which keeps it testable.
func run(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("yellowsocks-cli", flag.ContinueOnError)
	fs.SetOutput(stdout)

	configPath := fs.String("config", "", "Path to json/yaml configuration file")
	sppServer := fs.String("spp-server", "", "SPP remote server address (e.g. 1.2.3.4:8888)")
	sppProto := fs.String("spp-proto", "tcp", "SPP protocol (tcp, udp, kcp, quic)")
	sppKey := fs.String("spp-key", "123456", "SPP password / key")
	socks5Addr := fs.String("socks5", "127.0.0.1:1080", "Inbound SOCKS5 proxy listen address (TCP+UDP)")
	httpAddr := fs.String("http", "127.0.0.1:8080", "Inbound HTTP/HTTPS proxy listen address")
	dnsAddr := fs.String("dns", "127.0.0.1:53", "Inbound DNS UDP listen address")
	dohAddr := fs.String("doh", "127.0.0.1:8053", "Inbound DNS TCP DoH listen address")
	dotAddr := fs.String("dot", "", "Inbound DNS-over-TLS listen address (e.g. :853)")
	tlsCert := fs.String("tls-cert", "", "TLS certificate PEM for DoT")
	tlsKey := fs.String("tls-key", "", "TLS private key PEM for DoT")
	chinaDomains := fs.String("china-domains", "", "Path to china domain list file")
	gfwDomains := fs.String("gfw-domains", "", "Path to gfw domain list file")
	geoipFile := fs.String("geoip", "", "Path to GeoLite2-Country.mmdb")
	apiAddr := fs.String("api", "", "Statistics HTTP API listen address (e.g. 127.0.0.1:9090, empty disables)")
	apiToken := fs.String("api-token", "", "Statistics API token (random one is generated and logged when empty)")
	statsFile := fs.String("stats-file", "", "Path persisting statistics history (default stats.json when the API is on)")
	apiPProf := fs.Bool("api-pprof", false, "Serve /debug/pprof/ behind the admin token (off by default)")
	loglevel := fs.String("loglevel", "info", "Log level (debug, info, warn, error)")
	showVersion := fs.Bool("v", false, "Print version information and exit")
	showVersionLong := fs.Bool("version", false, "Print version information and exit")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *showVersion || *showVersionLong {
		fmt.Fprintln(stdout, version.String())
		return 0
	}

	var fileCfg *config.FileConfig
	if *configPath != "" {
		var err error
		fileCfg, err = config.LoadConfigFile(*configPath)
		if err != nil {
			fmt.Fprintf(stdout, "Error loading config file: %v\n\n", err)
			return 1
		}
	}

	level := loggo.LEVEL_INFO
	if fileCfg != nil && fileCfg.LogLevel != "" && loggo.NameToLevel(fileCfg.LogLevel) >= 0 {
		level = loggo.NameToLevel(fileCfg.LogLevel)
	} else if loggo.NameToLevel(*loglevel) >= 0 {
		level = loggo.NameToLevel(*loglevel)
	}
	loggo.Ini(loggo.Config{
		Level:     level,
		Prefix:    "yellowsocks-cli",
		NoLogFile: !launch.EnsureWritableWorkDir(),
	})

	cfg := core.EngineConfig{
		SPPServer:        *sppServer,
		SPPProto:         *sppProto,
		SPPKey:           *sppKey,
		SPPEncrypt:       "default",
		SPPCompress:      128,
		DNSListen:        *dnsAddr,
		DoHListen:        *dohAddr,
		DoTListen:        *dotAddr,
		TLSCertFile:      *tlsCert,
		TLSKeyFile:       *tlsKey,
		DirectDNS:        "1.1.1.1:53",
		RemoteDoH:        "https://1.1.1.1/dns-query",
		Socks5Listen:     *socks5Addr,
		HTTPListen:       *httpAddr,
		ChinaDomainsFile: *chinaDomains,
		GFWDomainsFile:   *gfwDomains,
		GeoIPFile:        *geoipFile,
		APIListen:        *apiAddr,
		APIToken:         *apiToken,
		APIDashboard:     true,
		APIPProf:         *apiPProf,
		StatsFile:        *statsFile,
	}

	if fileCfg != nil {
		cfg = fileCfg.MergeWithEngineConfig(cfg)
	}

	if cfg.APIListen != "" && cfg.StatsFile == "" {
		// History is only useful when something is watching it, and the
		// working directory is already guaranteed writable by launch.
		cfg.StatsFile = "stats.json"
	}

	if cfg.SPPServer == "" && len(cfg.SPPNodes) == 0 {
		fmt.Fprintln(stdout, "Usage: yellowsocks-cli -config <config.yaml> (or -spp-server <ip:port> -spp-key <key>)")
		fs.PrintDefaults()
		return 0
	}

	loggo.Info("Starting YellowSocks CLI (%s)...", version.String())

	engine := core.NewEngine(cfg)
	if err := engine.Start(); err != nil {
		loggo.Error("Failed to start engine: %v", err)
		return 1
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	loggo.Info("Received signal %v, exiting...", sig)

	_ = engine.Stop()
	loggo.Info("YellowSocks stopped successfully.")
	return 0
}
