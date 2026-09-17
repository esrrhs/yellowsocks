package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/esrrhs/gohome/common"
	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/yellowsocks/core"
	"github.com/esrrhs/yellowsocks/core/config"
)

func main() {
	defer common.CrashLog()

	configPath := flag.String("config", "", "Path to json/yaml configuration file")
	sppServer := flag.String("spp-server", "", "SPP remote server address (e.g. 1.2.3.4:8888)")
	sppProto := flag.String("spp-proto", "tcp", "SPP protocol (tcp, udp, kcp, quic)")
	sppKey := flag.String("spp-key", "123456", "SPP password / key")
	sppEncrypt := flag.String("spp-encrypt", "default", "SPP encryption method")
	sppCompress := flag.Int("spp-compress", 128, "SPP compression threshold")

	tunName := flag.String("tun-name", "tun0", "TUN interface name")
	tunIP := flag.String("tun-ip", "10.255.0.2", "TUN interface IP")
	tunGateway := flag.String("tun-gw", "10.255.0.1", "TUN default gateway")

	dnsListen := flag.String("dns-listen", "127.0.0.1:53", "DNS interceptor listen address")
	directDNS := flag.String("direct-dns", "1.1.1.1:53", "Direct upstream DNS server for direct lookups")
	dohURL := flag.String("doh-url", "https://1.1.1.1/dns-query", "Remote DoH endpoint routed through SPP")
	fakeIP := flag.Bool("fake-ip", true, "Enable Fake-IP mode for 0ms domain response (like Clash)")
	bypassApps := flag.String("bypass-apps", "dota2.exe,dota2,thunder.exe,xunlei.exe,baidunetdisk.exe", "Comma-separated list of process names to bypass proxy")
	autoRoute := flag.Bool("auto-route", true, "Automatically setup and restore system global routes")

	loglevel := flag.String("loglevel", "info", "Log level (debug, info, warn, error)")

	flag.Parse()

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
	if loggo.NameToLevel(*loglevel) >= 0 {
		level = loggo.NameToLevel(*loglevel)
	}
	loggo.Ini(loggo.Config{
		Level:  level,
		Prefix: "yellowsocks-cli",
	})

	var bypassList []string
	if *bypassApps != "" {
		for _, part := range strings.Split(*bypassApps, ",") {
			p := strings.TrimSpace(part)
			if p != "" {
				bypassList = append(bypassList, p)
			}
		}
	}

	cfg := core.EngineConfig{
		TunName:      *tunName,
		TunIP:        *tunIP,
		TunGateway:   *tunGateway,
		TunMask:      "255.255.255.0",
		MTU:          1500,
		SPPServer:    *sppServer,
		SPPProto:     *sppProto,
		SPPKey:       *sppKey,
		SPPEncrypt:   *sppEncrypt,
		SPPCompress:  *sppCompress,
		EnableFakeIP: *fakeIP,
		BypassApps:   bypassList,
		DNSListen:    *dnsListen,
		DirectDNS:    *directDNS,
		RemoteDoH:    *dohURL,
		SetAutoRoute: *autoRoute,
	}

	if fileCfg != nil {
		cfg = fileCfg.MergeWithEngineConfig(cfg)
	}

	if cfg.SPPServer == "" && len(cfg.SPPNodes) == 0 {
		fmt.Println("Usage: yellowsocks-cli -spp-server <ip:port> (or specify -config <path>)")
		flag.PrintDefaults()
		return
	}

	loggo.Info("Starting YellowSocks CLI Engine...")

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
