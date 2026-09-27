# yellowsocks

[<img src="https://img.shields.io/github/license/esrrhs/yellowsocks">](https://github.com/esrrhs/yellowsocks)
[<img src="https://img.shields.io/github/languages/top/esrrhs/yellowsocks">](https://github.com/esrrhs/yellowsocks)
[<img src="https://img.shields.io/github/v/release/esrrhs/yellowsocks">](https://github.com/esrrhs/yellowsocks/releases)
[<img src="https://img.shields.io/github/downloads/esrrhs/yellowsocks/total">](https://github.com/esrrhs/yellowsocks/releases)

Linux 代理，当前版本 **2.0.0**。本机提供 DNS、DoT、DoH、SOCKS5 和 HTTP 代理，需要转发的流量经 [SPP](https://github.com/esrrhs/spp) 送到远端。

2.0 只保留 Linux。不再提供 Windows、macOS、Android、iOS、OpenWrt 客户端，也不再创建虚拟网卡。

## 功能

- **DNS**：UDP 标准查询
- **DoH**：TCP，路径 `/dns-query`（RFC 8484，支持 GET 与 POST）
- **DoT**：可选的 DNS-over-TLS（RFC 7858）
- **SOCKS5**：TCP CONNECT 与 UDP ASSOCIATE
- **HTTP 代理**：普通 HTTP 请求与 HTTPS CONNECT
- **SPP 上游**：`tcp`、`udp`、`kcp`、`quic`，可配置多个节点并自动故障转移
- **分流**：按域名、CIDR 和 GeoIP 决定直连或走 SPP

国内域名和私网地址默认直连，其余走 SPP。

## 启动

把 `config.example.yaml` 复制成 `config.yaml`，填上 SPP 服务器后：

```bash
sudo ./yellowsocks-cli -config config.yaml
```

也可以直接传参数：

```bash
sudo ./yellowsocks-cli \
  -spp-server "your_spp_server:8888" \
  -spp-proto "tcp" \
  -spp-key "123456"
```

监听 `53` 端口需要 root。`Ctrl+C` 退出。

### 命令行参数

| 参数 | 说明 | 默认 |
| :--- | :--- | :--- |
| `-config` | YAML 或 JSON 配置文件 | 无 |
| `-spp-server` | 远端 SPP 地址 | 无 |
| `-spp-proto` | `tcp`、`udp`、`kcp`、`quic` | `tcp` |
| `-spp-key` | SPP 密钥 | `123456` |
| `-socks5` | 入站 SOCKS5 | `127.0.0.1:1080` |
| `-http` | 入站 HTTP 代理 | `127.0.0.1:8080` |
| `-dns` | DNS UDP | `127.0.0.1:53` |
| `-doh` | DoH TCP | `127.0.0.1:8053` |
| `-dot` | DoT 监听地址 | 空，不启用 |
| `-tls-cert` | DoT 证书 PEM | 空 |
| `-tls-key` | DoT 私钥 PEM | 空 |
| `-china-domains` | 国内域名列表 | 无 |
| `-gfw-domains` | 需要代理的域名列表 | 无 |
| `-geoip` | `GeoLite2-Country.mmdb` | 无 |
| `-loglevel` | `debug`、`info`、`warn`、`error` | `info` |
| `-v`、`-version` | 打印版本后退出 | |

配置文件里的同名项会覆盖命令行。多节点、入站认证、加密和压缩见 `config.example.yaml`。

### 配置示例

```yaml
spp_server: "1.2.3.4:8888"
spp_proto: "tcp"
spp_key: "123456"

socks5_listen: "127.0.0.1:1080"
http_listen: "127.0.0.1:8080"

dns_listen: "127.0.0.1:53"
doh_listen: "127.0.0.1:8053"
direct_dns: "114.114.114.114:53"
doh_url: "https://1.1.1.1/dns-query"
```

启用 DoT 时同时填写 `dot_listen`、`tls_cert` 和 `tls_key`。

## 编译

```bash
./pack.sh
```

产物在 `pack/`：

- `yellowsocks_linux_amd64.zip`
- `yellowsocks_linux_arm64.zip`
- `yellowsocks_linux_arm.zip`

每个压缩包里是 `yellowsocks-cli` 和 `config.example.yaml`。

## License

MIT
