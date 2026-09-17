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
  - **Pure Go User-space Stack**: Powered by gVisor netstack / tun2socks v2 without requiring CGO or MinGW toolchains.
- **Smart DNS Interception & Fake-IP Mode**:
  - Intercepts all outgoing UDP 53 DNS queries.
  - **Fake-IP Mode**: Delivers instant 0ms responses from `198.18.0.0/15` (RFC 2544 benchmark range) for fast connection establishment and elimination of local DNS leaks and poisoning.
  - **DoH Fallback**: Queries remote DoH resolvers (e.g. `https://1.1.1.1/dns-query`) securely routed through the SPP tunnel.
- **Universal Routing & Process/App-level Bypass**:
  - **App Bypass**: Automatically tracks active socket ports to process names (`PID -> Process`), directly bypassing high-concurrency apps, torrent clients, or latency-sensitive games (e.g., `dota2`, `thunder`, etc.).
  - **Custom Subnet & Domain Routing**: Supports loading custom direct CIDR lists and domain whitelists for any country or organization.
- **Modern Desktop UI & Live Dashboard**:
  - **Windows Desktop App**: Modern desktop GUI powered by native Windows Edge WebView2.
  - **Live Connection & Traffic Monitor**: Real-time tracking of upload/download bandwidth, active sessions, target destinations, and routing rule hits.
  - **One-Click System Proxy Injection**: Integrates with Windows Internet Settings registry to toggle system proxy on/off.

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

### 3. Android Mobile Client (`yellowsocks-android`)

The Android version provides system-wide transparent proxy without root permissions:
- **Zero Root Required**: Uses standard Android `VpnService` to capture packets and pass file descriptors directly to the Go core.
- **Pure Go User-space Stack**: Integrated `gVisor` + `tun2socks` userspace networking.
- **Native Per-App Bypass**: Directly excludes games/domestic apps at the OS level (`addDisallowedApplication`).
- **Live Compose Dashboard**: Built with modern Jetpack Compose for real-time speed monitoring, YAML config editing, and application bypass selection.

**Build AAR library**:
```bash
./build_android.sh
```
The resulting library `android/app/libs/yellowsocks.aar` will be automatically integrated into the Android project under `android/`.

---


## 🛠️ Build & Packaging

Build standalone archives for Linux (`amd64`/`arm64`) and Windows (`amd64`/`arm64`):
```bash
./pack.sh
```
Compiled archives will be generated in `pack/` and `pack.zip`.

---

## 📄 License
Released under the MIT License.
