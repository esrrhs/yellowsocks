# yellowsocks

[<img src="https://img.shields.io/github/license/esrrhs/yellowsocks">](https://github.com/esrrhs/yellowsocks)
[<img src="https://img.shields.io/github/languages/top/esrrhs/yellowsocks">](https://github.com/esrrhs/yellowsocks)
[<img src="https://img.shields.io/github/v/release/esrrhs/yellowsocks">](https://github.com/esrrhs/yellowsocks/releases)
[<img src="https://img.shields.io/github/downloads/esrrhs/yellowsocks/total">](https://github.com/esrrhs/yellowsocks/releases)

yellowsocks 是一个轻量级全平台全局虚拟网卡（TUN / Wintun）透明代理与智能分流工具，深度集成了 [esrrhs/spp](https://github.com/esrrhs/spp) 协议。

---

## 🌟 核心特性

- **集成 SPP**：直接对接 `esrrhs/spp` 客户端，支持经由 TCP、UDP、KCP、QUIC 等协议与远端 SPP 服务器通信并提供加密防嗅探能力。
- **系统级虚拟网卡代理**：
  - **Linux**：基于 TUN 设备拦截全局 IP 数据包，自动配置系统路由；
  - **Windows**：基于 Wintun 驱动接管全局流量。
- **DNS 智能劫持与缓存**：
  - 拦截所有系统 DNS 查询（UDP 53）；
  - **境内域名 / 局域网**：走国内 DNS（如 `223.5.5.5`）快速直连解析；
  - **境外域名**：采用 **DNS over HTTPS (DoH)**（如 `1.1.1.1`）经由 SPP 安全隧道远端解析，彻底免疫 DNS 投毒与污染；
  - 内置 DNS TTL 缓存与 IP-域名映射，用于快速判断目标流向。
- **智能境内外分流**：
  - 境内流量（包含私有网段、中国 IP 段及国内域名）自动**直连**；
  - 境外流量自动送入 **SPP 客户端隧道**加速传输。
- **双端形态**：
  - `yellowsocks-cli`：适用于 Linux / 服务器 / 软路由的命令行模式。
  - `yellowsocks-gui`：适用于 Windows 桌面端的控制器。

---

## 🚀 启动与使用方式

### 1. Linux 命令行 (`yellowsocks-cli`)

无需繁琐的手动 iptables 配置，以 root 权限运行即可一键接管全局流量：

```bash
sudo ./yellowsocks-cli \
  -spp-server "your_spp_server_ip:8888" \
  -spp-proto "tcp" \
  -spp-key "123456" \
  -china-dns "223.5.5.5:53" \
  -doh-url "https://1.1.1.1/dns-query" \
  -auto-route=true
```

#### 参数说明：
| 参数 | 说明 | 默认值 |
| :--- | :--- | :--- |
| `-spp-server` | 远端 SPP 服务器地址 (必填) | 无 |
| `-spp-proto` | SPP 传输协议 (`tcp`, `udp`, `kcp`, `quic`) | `tcp` |
| `-spp-key` | SPP 密钥 | `123456` |
| `-spp-encrypt` | SPP 加密方式 | `default` |
| `-spp-compress` | SPP 压缩阈值 | `128` |
| `-tun-name` | 虚拟网卡设备名 | `tun0` |
| `-china-dns` | 境内直连解析 DNS | `223.5.5.5:53` |
| `-doh-url` | 境外 DoH 上游 | `https://1.1.1.1/dns-query` |
| `-auto-route` | 是否自动设置与清理系统路由 | `true` |

退出时只需按 `Ctrl + C`，程序会自动清理路由规则并释放 TUN 设备。

---

### 2. Windows (`yellowsocks-gui`)

1. 下载对应平台的 `yellowsocks-gui.exe` 与对应架构的 `wintun.dll`（放置在同目录）。
2. 右键“以管理员身份运行”：
```cmd
yellowsocks-gui.exe -spp-server "your_spp_server_ip:8888" -spp-proto "tcp" -spp-key "123456"
```
3. 程序会自动调用 Wintun 初始化网络适配器、劫持 DNS 并根据分流规则智能路由。

---

## 🛠️ 构建打包

编译所有平台（Linux amd64/arm64、Windows amd64/arm64）：
```bash
./pack.sh
```
产物将输出在 `pack/` 目录以及 `pack.zip` 中。
