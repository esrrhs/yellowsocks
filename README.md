# yellowsocks

[<img src="https://img.shields.io/github/license/esrrhs/yellowsocks">](https://github.com/esrrhs/yellowsocks)
[<img src="https://img.shields.io/github/languages/top/esrrhs/yellowsocks">](https://github.com/esrrhs/yellowsocks)
[<img src="https://img.shields.io/github/v/release/esrrhs/yellowsocks">](https://github.com/esrrhs/yellowsocks/releases)
[<img src="https://img.shields.io/github/downloads/esrrhs/yellowsocks/total">](https://github.com/esrrhs/yellowsocks/releases)

`yellowsocks` is a cross-platform, system-level transparent proxy and intelligent routing engine based on virtual network interfaces (TUN / Wintun), seamlessly integrated with the [esrrhs/spp](https://github.com/esrrhs/spp) protocol.

---

## 🌟 Key Features

- **Integrated SPP Protocol**: Directly connects to `esrrhs/spp` clients over TCP, UDP, KCP, or QUIC with built-in encryption and compression.
- **Multi-Node Failover**: Supports configuring multiple upstream SPP server nodes with concurrent heartbeat latency checks (15s) and automatic failover.
- **System-wide Virtual Network Interface (TUN / Wintun)**:
  - **Linux**: Intercepts L3 IP packets via TUN devices and automatically manages system routing tables (`0.0.0.0/1` and `128.0.0.0/1`).
  - **Windows**: High-performance packet capture and routing through `Wintun`.
  - **macOS**: Manages `utun` TUN interfaces via `/sbin/route` with system-wide routing.
  - **Pure Go User-space Stack**: Powered by gVisor netstack / tun2socks v2 without requiring CGO or MinGW toolchains.
- **Smart DNS Interception & Fake-IP Mode**:
  - Intercepts all outgoing UDP 53 DNS queries.
  - **Fake-IP Mode**: Powered by `gohome/dns/fakeip` delivering instant 0ms responses from `198.18.0.0/15` (RFC 2544 benchmark range) with synchronized TTL expiration and reverse lookup for fast connection establishment and elimination of local DNS leaks and poisoning.
  - **DoH Fallback**: Queries remote DoH resolvers (e.g. `https://1.1.1.1/dns-query`) securely routed through the SPP tunnel.
- **Universal Routing & Process/App-level Bypass**:
  - **App Bypass**: Automatically tracks active socket ports to process names (`PID -> Process`), directly bypassing high-concurrency apps, torrent clients, or latency-sensitive games.
  - **Custom Subnet & Domain Routing**: Supports loading custom direct CIDR lists and domain whitelists for any country or organization.
- **Modern Desktop UI & Live Dashboard**:
  - **Windows Desktop App**: Modern desktop GUI powered by native Windows Edge WebView2 with system tray.
  - **macOS Desktop App**: Native desktop control panel window (Cocoa + WebKit WKWebView) with menu-bar tray icon and auto-minimize support.
  - **Live Connection & Traffic Monitor**: Real-time tracking of upload/download bandwidth, active sessions, target destinations, and routing rule hits.
  - **One-Click System Proxy**: Toggle system-level SOCKS5 proxy on/off (Windows Internet Settings / macOS `networksetup`).
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
