package sppclient

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/esrrhs/gohome/loggo"
	"github.com/esrrhs/spp/proxy"
)

// Config SPP 客户端配置
type Config struct {
	ServerProto string // 主通道协议: tcp, udp, kcp, quic 等
	Server      string // 远端 SPP 服务器地址: ip:port
	Key         string // 连接密钥
	Encrypt     string // 加密密钥
	Compress    int    // 压缩阈值 (0表示不压缩)
	LocalSocks5 string // 本地监听的 Socks5 端口，如 127.0.0.1:10808
	Name        string
}

// Client 封装 spp proxy.Client 并提供与内核对接的拨号能力
type Client struct {
	cfg         *Config
	sppProxy    *proxy.Client
	localSocks5 string
	mu          sync.Mutex
	closed      bool
}

// NewClient 启动 SPP socks5_client 模式
func NewClient(cfg *Config) (*Client, error) {
	if cfg.Server == "" {
		return nil, fmt.Errorf("spp server address cannot be empty")
	}
	if cfg.ServerProto == "" {
		cfg.ServerProto = "tcp"
	}
	if cfg.LocalSocks5 == "" {
		// 动态分配或默认端口
		cfg.LocalSocks5 = "127.0.0.1:10808"
	}
	if cfg.Name == "" {
		cfg.Name = "yellowsocks_spp"
	}

	proxyCfg := proxy.DefaultConfig()
	proxyCfg.Key = cfg.Key
	// "default" 与空值都表示关闭加密，与 spp -encrypt 留空的行为一致。
	// spp 会拒绝 default/password 这类弱密钥，不能原样传进去。
	enc := strings.TrimSpace(cfg.Encrypt)
	if strings.EqualFold(enc, "default") {
		enc = ""
	}
	proxyCfg.Encrypt = enc
	proxyCfg.Compress = cfg.Compress

	// 构造 NewClient 参数
	// 启动 socks5_client 模式:
	// clienttypestr: SOCKS5
	// proxyproto: []string{"tcp"}
	// fromaddr: []string{cfg.LocalSocks5}
	// toaddr: []string{""} (socks5无需固定对端)
	fromAddrs := []string{cfg.LocalSocks5}
	proxyProtos := []string{"tcp"}
	toAddrs := []string{""}

	loggo.Info("[SPP] Starting SPP Client connecting to %s via %s, local socks5: %s", cfg.Server, cfg.ServerProto, cfg.LocalSocks5)

	c, err := proxy.NewClient(
		proxyCfg,
		[]string{strings.ToLower(cfg.ServerProto)},
		[]string{cfg.Server},
		cfg.Name,
		"SOCKS5",
		proxyProtos,
		fromAddrs,
		toAddrs,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to init SPP client: %w", err)
	}

	// 等待 socks5 端口监听就绪
	client := &Client{
		cfg:         cfg,
		sppProxy:    c,
		localSocks5: cfg.LocalSocks5,
	}

	// 快速探测 localSocks5 是否在监听
	go func() {
		for i := 0; i < 20; i++ {
			conn, err := net.DialTimeout("tcp", cfg.LocalSocks5, 200*time.Millisecond)
			if err == nil {
				conn.Close()
				loggo.Info("[SPP] Local socks5 listener ready on %s", cfg.LocalSocks5)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	return client, nil
}

// Socks5Addr 获取本地 socks5 地址
func (c *Client) Socks5Addr() string {
	return c.localSocks5
}

// Close 关闭客户端
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.sppProxy != nil {
		c.sppProxy.Close()
	}
	return nil
}
