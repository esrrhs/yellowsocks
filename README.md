# yellowsocks

[<img src="https://img.shields.io/github/license/esrrhs/yellowsocks">](https://github.com/esrrhs/yellowsocks)
[<img src="https://img.shields.io/github/languages/top/esrrhs/yellowsocks">](https://github.com/esrrhs/yellowsocks)
[<img src="https://img.shields.io/github/v/release/esrrhs/yellowsocks">](https://github.com/esrrhs/yellowsocks/releases)
[<img src="https://img.shields.io/github/downloads/esrrhs/yellowsocks/total">](https://github.com/esrrhs/yellowsocks/releases)

`yellowsocks` is a unified cross-platform proxy and intelligent routing suite, combining the capabilities of **SPP**, **YellowDNS**, and **SocksFilter**:

1. **High-Performance DNS Engine**: Listens on UDP (standard DNS), TCP DoH (RFC 8484), and optional DoT (RFC 7858 / Android Private DNS) with intelligent domestic/remote split routing, GeoIP anti-poisoning, and optional Fake-IP mode (`198.18.0.0/15`).
2. **Inbound SOCKS5 Proxy**: Listens on a local port providing RFC 1928 SOCKS5 service with concurrent **TCP CONNECT** and **UDP ASSOCIATE** forwarding, coupled with intelligent domestic direct vs. remote proxy routing.
3. **Inbound HTTP/HTTPS Proxy**: Listens on a local port providing standard HTTP method proxying and HTTPS **CONNECT** tunneling, sharing the same intelligent routing rules.
4. **SPP Upstream Multiplexing**: All traffic marked for proxying (TUN, SOCKS5 TCP/UDP, HTTP, and remote DNS queries) is multiplexed and forwarded to remote SPP servers over TCP, UDP, KCP, or QUIC with encryption and compression.
5. **Full-Machine Transparent Capture (TUN)**: Leverages virtual network interfaces (TUN / Wintun) and user-space TCP/IP stacks (gVisor netstack / tun2socks v2) to transparently proxy all machine traffic.

---

## 🌟 Key Features

- **Integrated SPP Protocol**: Connects to `esrrhs/spp` servers over TCP, UDP, KCP, or QUIC with built-in encryption and compression.
- **Multi-Node Failover**: Supports configuring multiple upstream SPP server nodes with concurrent heartbeat latency checks (15s) and automatic failover.
- **Fast & Accurate DNS Service (UDP + DoH + DoT)**:
  - **UDP Port**: Provides standard, fast, anti-poisoning DNS services.
  - **TCP Port**: Provides standard DoH (DNS-over-HTTPS / RFC 8484) services at `/dns-query` (supporting both GET `?dns=<base64url>` and POST `application/dns-message`).
  - **DoT Port**: Optional DNS-over-TLS (RFC 7858) for Android Private DNS / 专用 DNS (requires public CA cert).
  - **Fake-IP Mode**: Powered by `gohome/dns/fakeip` delivering instant 0ms responses with synchronized TTL and reverse mapping.
- **Dual Inbound Proxies with Smart Routing**:
  - **SOCKS5 Proxy**: Supports both TCP and SOCKS5 UDP Associate forwarding.
  - **HTTP/HTTPS Proxy**: Supports standard HTTP proxying and HTTPS CONNECT tunneling.
  - **Smart Routing (Direct vs Proxy)**: Automatically decides whether traffic should go directly or through SPP based on process names, domain lists (China/GFW), CIDRs, and GeoIP.
- **System-wide Virtual Network Interface (TUN / Wintun)**:
  - **Linux**: Intercepts L3 IP packets via TUN devices and automatically manages system routing tables (`0.0.0.0/1` and `128.0.0.0/1`).
  - **Windows**: High-performance packet capture and routing through `Wintun`.
  - **macOS**: Manages `utun` TUN interfaces via `/sbin/route` with system-wide routing.
  - **Pure Go User-space Stack**: Powered by gVisor netstack / tun2socks v2 without requiring CGO or MinGW toolchains.
- **Process/App-level Bypass**: Automatically tracks active socket ports to process names (`PID -> Process`), directly bypassing high-concurrency apps, torrent clients, or latency-sensitive games.
- **Modern Desktop UI & Live Dashboard**:
  - **Windows Desktop App**: Modern desktop GUI powered by native Windows Edge WebView2 with system tray.
  - **macOS Desktop App**: Native desktop control panel window (Cocoa + WebKit WKWebView) with menu-bar tray icon and auto-minimize support.
  - **Live Connection & Traffic Monitor**: Real-time tracking of upload/download bandwidth, active sessions, target destinations, and routing rule hits.
- **Comprehensive Cross-Platform Matrix**:
  - **Desktop**: Windows (x86_64, arm64), macOS (Apple Silicon, Intel), Linux (CLI).
  - **Mobile**: Android (Jetpack Compose), iOS & iPadOS (SwiftUI + NetworkExtension).
  - **Routers**: OpenWrt / Embedded Linux (x86, ARM64, ARMv7, MIPS, MIPSLE).

---

## 🚀 Quick Start

### 1. Linux Command Line (`yellowsocks-cli`)

Run with root privileges to automatically create the TUN interface and configure system routing:

```bash
# Recommended: Start with a configuration file (YAML / JSON):
sudo ./yellowsocks-cli -config config.yaml

# Or start directly with minimal flags:
sudo ./yellowsocks-cli \
  -spp-server "your_spp_server_ip:8888" \
  -spp-proto "tcp" \
  -spp-key "123456"
```

#### CLI Options:
| Flag | Description | Default |
| :--- | :--- | :--- |
| `-config` | Path to YAML or JSON configuration file | None |
| `-spp-server` | Remote SPP server address (`ip:port`) | None |
| `-spp-proto` | SPP protocol (`tcp`, `udp`, `kcp`, `quic`) | `tcp` |
| `-spp-key` | SPP authentication key / password | `123456` |
| `-socks5` | Inbound SOCKS5 proxy listen address (TCP+UDP) | `127.0.0.1:1080` |
| `-http` | Inbound HTTP/HTTPS proxy listen address | `127.0.0.1:8080` |
| `-dns` | Inbound DNS UDP listen address | `127.0.0.1:53` |
| `-doh` | Inbound DNS TCP DoH listen address | `127.0.0.1:8053` |
| `-dot` | Inbound DNS-over-TLS listen address | _(empty)_ |
| `-tls-cert` | TLS certificate PEM for DoT | _(empty)_ |
| `-tls-key` | TLS private key PEM for DoT | _(empty)_ |
| `-disable-tun` | Disable TUN device (run proxies and DNS only) | `false` |
| `-china-domains` | Path to China domain list file | None |
| `-gfw-domains` | Path to GFW domain list file | None |
| `-geoip` | Path to `GeoLite2-Country.mmdb` | None |
| `-loglevel` | Log level (`debug`, `info`, `warn`, `error`) | `info` |

> [!TIP]
> Detailed settings such as virtual TUN IP, Fake-IP mode, DNS upstream/DoH endpoints, multi-node automatic failover, and process bypass lists are cleanly managed via the configuration file (`config.example.yaml`).

Press `Ctrl + C` to gracefully stop the engine, delete TUN routes, and restore system networking.

---

### 2. Windows Desktop GUI (`yellowsocks-gui`)

1. Download `yellowsocks-gui.exe` and ensure `wintun.dll` is located in the same directory.
2. Run as Administrator with a config file or server parameters:
```cmd
yellowsocks-gui.exe -config config.yaml
```
3. A modern desktop control panel will open showing real-time speeds, active connections, and routing events.

---

### 3. macOS Desktop GUI (`yellowsocks-gui`)

1. Build natively on a macOS host (CGO is required for systray & WebKit):
```bash
./build_macos.sh
# or build binary directly:
# CGO_ENABLED=1 go build -ldflags="-s -w" -o yellowsocks-gui ./cmd/yellowsocks-gui
```
2. Run with root privileges (required for TUN interface creation and route management):
```bash
sudo ./yellowsocks-gui -config config.yaml
```
3. The app launches with a **menu-bar tray icon** and presents a **native macOS desktop control panel** (powered by Cocoa + WebKit WKWebView, zero external runtime required).
4. Closing the window minimizes/hides it back to the menu-bar; clicking **Open Control Panel** in the tray menu instantly brings it back to the foreground.

#### Tray Menu Items
| Item | Description |
| :--- | :--- |
| **Start / Stop TUN Proxy** | Enables or disables the virtual TUN tunnel |
| **System Proxy: OFF / ON** | Toggles system-wide SOCKS5 proxy via `networksetup` |
| **Fake-IP: ENABLED / DISABLED** | Toggles Fake-IP DNS mode |
| **Open Control Panel** | Shows the native macOS control panel window |
| **Quit** | Gracefully stops the engine and exits |

---

### 4. Android Mobile Client (`yellowsocks-android`)

The Android version provides system-wide transparent proxy without root permissions:
- **Zero Root Required**: Uses standard Android `VpnService` to capture packets and pass file descriptors directly to the Go core.
- **Pure Go User-space Stack**: Integrated `gVisor` + `tun2socks` userspace networking.
- **TCP + UDP Forwarding**: TUN-path TCP/UDP with Fake-IP reverse lookup and SPP SOCKS5 UDP ASSOCIATE; Direct sockets use `VpnService.protect` to avoid routing loops.
- **Native Per-App Bypass**: Directly excludes apps at the OS level (`addDisallowedApplication`).
- **Live Compose Dashboard**: Built with modern Jetpack Compose for real-time speed monitoring, YAML config editing, and application bypass selection.

**Build AAR library**:
```bash
./build_android.sh
```
The resulting library `android/app/libs/yellowsocks.aar` will be automatically integrated into the Android project under `android/`.

---

### 5. iOS & iPadOS Mobile Client (`YellowSocks-iOS`)

The iOS & iPadOS client provides seamless system-wide transparent proxy without jailbreak:
- **Native NetworkExtension Architecture**: Built on Apple's official `NEPacketTunnelProvider` to capture L3 IP packets directly into the Go core.
- **Pure Go User-space Stack**: Full `gVisor` + `tun2socks` userspace network stack with SPP multi-node automatic failover.
- **Responsive SwiftUI Design**: Adapts beautifully to both iPhone and iPad screens with dynamic traffic dials, active SPP latency monitors, and built-in YAML configuration editor.
- **Background Keepalive & Reconnect**: Integrates system VPN reconnect triggers and AppGroup shared memory statistics.

**Build XCFramework on macOS**:
```bash
./build_ios.sh
```
The script outputs `ios/Frameworks/YellowSocks.xcframework`. Then open `ios/YellowSocks.xcodeproj` in Xcode and click **Run** to install directly to your connected iPhone / iPad.

---

### 6. OpenWrt & Embedded Linux Routers

Run YellowSocks directly on your router as a **transparent gateway**, giving all connected LAN devices (Apple TV, game consoles, smart TVs, IoT, phones) instant acceleration without installing any client software:
- **Broad Hardware Architecture Support**:
  - **x86 Soft Routers**: `linux/amd64` (J1900, N5105, PVE, ESXi)
  - **ARM64 Routers**: `linux/arm64` (NanoPi R2S/R4S/R5S, Raspberry Pi 4/5)
  - **ARMv7 Routers**: `linux/arm` (ASUS / Netgear routers)
  - **MIPSLE Routers**: `linux/mipsle` with softfloat (MediaTek MT7621, MT7620, Xiaomi routers)
  - **MIPS Routers**: `linux/mips` with softfloat (Atheros / Qualcomm routers)
- **Native OpenWrt Service Integration**:
  - Standard `procd` service management with automatic respawn on failure.
  - Integrated `iptables` rules for automatic LAN packet forwarding and masquerade.
  - Standard UCI configuration `/etc/config/yellowsocks`.

**Quick Install on OpenWrt**:
```bash
# 1. Download and extract the matching architecture archive (e.g., mipsle for MT7621)
# 2. Run the one-click installer:
cd openwrt && ./install.sh

# 3. Configure your server and start:
vi /etc/yellowsocks/config.yaml
uci set yellowsocks.main.enabled='1'
uci commit yellowsocks
/etc/init.d/yellowsocks start
```

---


## 🛠️ Build & Packaging

Build standalone archives for all architectures (Linux `amd64`/`arm64`/`armv7`/`mipsle`/`mips` and Windows `amd64`/`arm64`):
```bash
./pack.sh
```
Compiled archives will be generated in `pack/` and `pack.zip`.

> [!NOTE]
> **macOS GUI builds** require CGO (for systray Objective-C bindings) and must be compiled natively on a macOS host. See the commented-out darwin section in `pack.sh`.

---

## 📄 License
Released under the MIT License.
