# yellowsocks

[<img src="https://img.shields.io/github/license/esrrhs/yellowsocks">](https://github.com/esrrhs/yellowsocks)
[<img src="https://img.shields.io/github/languages/top/esrrhs/yellowsocks">](https://github.com/esrrhs/yellowsocks)
[<img src="https://img.shields.io/github/v/release/esrrhs/yellowsocks">](https://github.com/esrrhs/yellowsocks/releases)
[<img src="https://img.shields.io/github/downloads/esrrhs/yellowsocks/total">](https://github.com/esrrhs/yellowsocks/releases)

Linux 代理。本地提供 DNS、DoT、DoH、SOCKS5 和 HTTP 代理，需要转发的流量经 [SPP](https://github.com/esrrhs/spp) 送到远端。

- **DNS**：UDP 标准 DNS
- **DoH**：TCP，`/dns-query`（RFC 8484）
- **DoT**：可选的 DNS-over-TLS（RFC 7858）
- **SOCKS5**：TCP CONNECT 与 UDP ASSOCIATE
- **HTTP 代理**：普通 HTTP 与 HTTPS CONNECT
- **SPP 上游**：TCP、UDP、KCP、QUIC，支持多节点故障转移
- **分流**：按域名、CIDR、GeoIP 决定直连或走 SPP

## 启动

```bash
sudo ./yellowsocks-cli -config config.yaml
```

或：

```bash
sudo ./yellowsocks-cli \
  -spp-server "your_spp_server:8888" \
  -spp-proto "tcp" \
  -spp-key "123456"
```

监听 53 端口需要 root。不需要虚拟网卡。

| 参数 | 说明 | 默认 |
| :--- | :--- | :--- |
| `-config` | YAML 或 JSON 配置 | 无 |
| `-spp-server` | 远端 SPP 地址 | 无 |
| `-spp-proto` | `tcp`、`udp`、`kcp`、`quic` | `tcp` |
| `-spp-key` | SPP 密钥 | `123456` |
| `-socks5` | 入站 SOCKS5 | `127.0.0.1:1080` |
| `-http` | 入站 HTTP 代理 | `127.0.0.1:8080` |
| `-dns` | DNS UDP | `127.0.0.1:53` |
| `-doh` | DoH TCP | `127.0.0.1:8053` |
| `-dot` | DoT 监听地址 | 空 |
| `-tls-cert` | DoT 证书 | 空 |
| `-tls-key` | DoT 私钥 | 空 |
| `-loglevel` | 日志级别 | `info` |

更多选项见 `config.example.yaml`。`Ctrl+C` 退出。

## 编译

```bash
./pack.sh
```

产物在 `pack/`：`linux/amd64`、`linux/arm64`、`linux/arm`。

## License

MIT
