# YellowSocks for Windows — Feature Guide

**English** | [简体中文](WINDOWS.zh-CN.md)

The Windows client is the same YellowSocks engine behind a desktop UI, a system
tray icon and an optional TUN capture driver. `config.yaml` has exactly the
format the CLI reads, and the statistics API is identical, so a setup can move
between the two freely. This guide walks through every feature of the app; the
CLI, the config format and the API endpoints themselves are documented in
[USAGE.md](USAGE.md).

- [Installation and layout](#installation-and-layout)
- [The window](#the-window)
- [Home](#home)
- [Connections](#connections)
- [Nodes](#nodes)
- [Logs](#logs)
- [Settings](#settings)
- [Statistics](#statistics)
- [About](#about)
- [System tray](#system-tray)
- [System proxy](#system-proxy)
- [TUN mode](#tun-mode)
- [Process attribution](#process-attribution)
- [The watchdog pair and updates](#the-watchdog-pair-and-updates)
- [Data files and logs](#data-files-and-logs)
- [Statistics API and dashboard](#statistics-api-and-dashboard)
- [Known limits](#known-limits)

## Installation and layout

`yellowsocks-gui.exe`, `yellowsocks-watchdog.exe`, `wintun.dll` and
`config.yaml` in one directory are a complete installation. Everything the app
writes — `settings.json`, `stats.json`, the log files — lands in the same
directory (portable mode). When that directory is read-only (Program Files, a
read-only USB stick) the per-user directory `%APPDATA%\YellowSocks` is used
instead, and a fresh portable directory adopts the data files from there once.

- **First launch** writes a commented `config.yaml` in the CLI format. Edit it
  from the Settings page, or with any text editor.
- **Single instance.** Starting the exe again does not open a second copy — the
  running window is brought to the front instead.
- **Closing the window** hides it to the tray; the engine keeps running. The
  tray icon is the way back in (left click or the menu item).
- **Launch at login** registers a scheduled task (`YellowSocks`) that starts
  the app at the highest available privileges with `--hidden`, so the window
  opens in the tray without a UAC prompt.
- **Elevation.** TUN mode needs administrator rights. Switching it on from an
  unelevated window asks for UAC once; on confirmation the app restarts as an
  elevated copy and brings the adapter up there (the engine starts again in
  the new process; saved settings carry over).

## The window

The sidebar lists **主页, 连接, 节点, 日志, 设置, 统计, 关于** in that order.
Pages stay alive in the background: each one pauses its polling while hidden
and takes a fresh sample the moment it is shown, so switching tabs is instant
and the first paint is never stale.

The footer holds two persistent choices that apply to the whole app (tray
included):

- **Theme** — 跟随 (follow the OS), 深色 (dark), 浅色 (light).
- **Language** — 中 / EN. The choice is stored in `settings.json` and drives
  both the window and the tray menu, which re-render within a second.

The top-right button starts or stops the proxy engine (启动代理 / 停止代理,
with `处理中…` while a start or stop is in flight). It turns red while the
engine is running. Engine failures are reported on the Home page, not here.

## Home

**Routing mode** — a segmented control picks how the engine decides:

| Mode | Behavior |
| --- | --- |
| 规则 | 国内直连、国外走代理: rules, GeoIP and domain lists decide per destination (the default). |
| 全局 | 除局域网/本机外全部走代理: everything except LAN and loopback goes through the upstream. |
| 直连 | 全部直连,不经过代理: everything goes out directly; useful to compare or to keep the port layout while bypassing. |

When the self-check has seen a working exit, a chip next to the mode shows
`出口 {latency} · {IP} · {country} · {city}` — the address your proxied traffic
actually leaves from, measured through the real path.

**Switches** — each is a read-back toggle (the app shows what the backend
confirms, not what was clicked):

| Switch | What it does |
| --- | --- |
| 系统透明代理 | Registers the local HTTP/SOCKS endpoints in the Windows per-user proxy settings and refreshes WinINET, so browsers and WinINET apps use the proxy immediately. Registers nothing while TUN captures everything (see below). |
| 自动连接 | Start the engine with the app (including a launch-at-login start). |
| 开机启动 | Registers/unregisters the elevated logon task. The OS registry is the source of truth; the switch reflects it. |
| TUN 模式 | Brings the capture adapter up (next to the switch: 建立中… while starting, 自动重试中… while the retry ladder runs, 失败,点重试 after it gives up; the switch turns red on failure). |
| 系统代理守卫 | Keeps the system proxy registration pointed at this app. While on, another program overwriting the registry is repaired within 30 seconds — which also means you cannot switch the system proxy off from the Windows settings page while it is enabled. Opt-in, off by default. |

Failed toggles raise a banner with the backend's error text instead of failing
silently.

**Metric cards** — 活动连接 (with the lifetime maximum as the gauge ceiling,
split into 国内 / 国外), 上传速率 and 下载速率 (same split, ceiling = lifetime
peak), 本次上传, 本次下载, 运行时长.

**近 30 分钟流量** — four curves: 上传·国内, 上传·国外 (dashed), 下载·国内,
下载·国外 (dashed), one-second resolution, kept across engine restarts for the
window's lifetime.

## Connections

Three tabs, all polling once a second:

**连接 — live sessions.** Each row is one active proxied connection:

| Column | Content |
| --- | --- |
| 域名 | The domain (recovered from the DNS hijack or sniffed from TLS SNI) with the port, and the connected address underneath. A bare address has no line underneath. |
| 客户端 | The owning application (icon + exe name) when attribution succeeded, the client address otherwise. |
| 决策 | 直连 / 代理, matching the routing decision for that connection. |
| 节点 | The upstream node that carried it (empty for direct connections). |
| 上传 / 下载 | Live rate (primary) and the bytes this connection has carried so far. |
| 时长 | Time since the connection was accepted. |

Above the table: a search box (domain, client, process, node), 全部 / 代理 /
直连 chips with per-filter counts, and `{n} / {m} 条` where `m` is the
engine's own active count — the returned list is capped, and the counter says
so instead of pretending the table holds everything.

**Every column sorts.** Click a header: text columns go A→Z first, numeric
columns largest-first, clicking the same header flips the direction, and the
active header shows ▴/▾. Rows with equal values keep a stable order, so the
one-second refresh never reshuffles the table under the cursor.

**应用 — per process.** One row per application with traffic: 7 sortable
columns (应用, 上传, 下载, 总流量, 国内, 国外, 连接数). 国内/国外 show that
side's live rates plus its cumulative bytes.

**目标 — per destination.** Active connections grouped the same way the
Statistics page groups them (vendor table, then registrable domain, with a
pinned 未识别地址 bucket for bare addresses), showing each destination's live
rates and carried bytes. Click a row to expand the member hostnames (largest
first, top 8 shown with a count of the rest). Sort chips above the list order
the rows by 名称, 速率 (the default, busiest first), ↑上传, ↓下载 or 累计; the
未识别地址 bucket stays pinned last whatever the order.

## Nodes

The upstream list with priority-based failover: lower priority numbers win,
the engine preempts back to a better tier when a node recovers, and within one
tier higher bandwidth is preferred. SOCKS5 nodes dial directly with their own
credentials; SPP nodes speak the tunnel protocols of the CLI.

- **添加节点 / 保存.** 保存 validates and writes the `nodes:` list into
  `config.yaml` (legacy flat `spp_*` keys are stripped); changes take effect
  on the next engine start. 保存 stays disabled until something changed.
- **Row contents.** Name (未命名节点 when blank), type chip (SOCKS5 服务器 /
  SPP 信道), address, a status: green dot with `{ms}` after a real SOCKS5
  handshake + HTTP GET probe (every 3 s), 不可用 when the probe fails,
  待探测 before the first round.
- **Editing.** Click a row to expand the form: 名称, 地址; SOCKS5 adds 端口,
  用户名, 密码 (leave empty for no auth); SPP adds 协议 (tcp, udp, rudp,
  ricmp, kcp, quic, rhttp, rdns, pingtunnel) and 密钥; both share 优先级 and
  带宽 (Mbps). Validation errors name the offending node (第 {n} 个节点…).
- **节点流量.** A table of per-node traffic: 节点, 连接数, 上传, 下载,
  最后活动, with a 合计 footer row.

## Logs

The engine's log files, tailed live:

- Level filter 全部 / INFO / WARN / ERROR / DEBUG, and a search box matching
  message, level and file name (case-insensitive).
- Up to 500 newest lines, re-read every second while the page is visible.
- Follow mode is implicit: the view sticks to the bottom while you are near
  it (< 40 px), scrolling up pauses following, scrolling back to the bottom
  resumes. Hovering a line shows the full raw entry.
- The footer names the log directory and the refresh cadence.

## Settings

The whole `config.yaml`, editable two ways: a **表单** view with per-section
tabs, and a **YAML 源码** editor (comments preserved). Both save to the same
file; the engine reads it on start or restart, and the page says so after
every save.

| Tab | Fields |
| --- | --- |
| 本地监听 | 允许局域网访问 (single checkbox flipping the listen hosts between loopback and `0.0.0.0`), SOCKS5 / HTTP / DNS / DoH / DoT listen addresses, IPv6 出站. |
| DNS 上游 | 直连 DNS, 远程 DoH URL. |
| 路由分流 | 直连域名, 直连网段, 跳过国家, GeoIP 数据库, 国内域名表, GFW 域名表. |
| 认证与 TLS | Proxy username/password, DoT certificate and key. |
| API 与统计 | API listen address, API token, read-only token, pprof switch, embedded dashboard switch, statistics file, history hours, log level. |

Form saves merge the active tab onto a freshly parsed YAML: keys you did not
touch — including ones this version does not know — survive, empty fields
delete their key, and each tab remembers whether it has unsaved edits (red
dot). 重新加载 discards unsaved edits. YAML mode saves the editor text
verbatim, and parse errors are shown instead of written.

The upstream server list is not part of this page; it lives on **节点**.

## Statistics

Three tabs over the engine's lifetime record (`stats.json`, surviving reboots
and engine restarts):

- **历史监控** — 累计流量 for 国内 / 国外 / 合计, 峰值速度, 平均速度 (over
  accumulated on-time) and 最大并发连接, plus 清零 to reset the permanent
  record.
- **按目标归类** — traffic grouped by destination, split 国内/国外: each row
  shows share %, bytes and ↑/↓, expanding into the largest member domains.
- **按应用程序** — traffic per application (TCP only: UDP carries no process
  information), with icons, share and ↑/↓.

## About

Version, git commit, build time and the runtime line (`os/arch · go version`),
plus a button to the GitHub project page.

## System tray

The tray mirrors the dashboard, refreshed once a second from the same state
the window reads — it cannot disagree with the window:

- Two disabled status lines: the current `↓ rate  ↑ rate` and the active
  connection count.
- 显示主界面 (a left click on the icon does the same), 启动/停止代理,
  系统透明代理, TUN 模式 toggles, the routing mode as a checked submenu
  (规则/全局/直连), and 退出.
- The tooltip carries the live rates, and the exit address once the self-check
  has seen one — "is my traffic leaving from where the rules promise" is
  answerable without opening the window.
- The menu language follows the window: switching to EN in the window renames
  the menu within a second.
- 退出 takes the watchdog down with it (`watchdog.stop`) so the mutual-restart
  pair exits together instead of the guard resurrecting the app.
- The icon is removed on exit; if Windows leaves a ghost behind (it disappears
  when the pointer crosses it), that is Explorer's icon cache, not a running
  app. A tray health watchdog pings the tray's message loop every 5 s; if it
  stops answering for more than a few seconds it writes the goroutine stacks
  to `tray-hang-*.log` so the hang can be diagnosed.

## System proxy

The 系统透明代理 switch writes the Windows per-user WinINET registration:
`ProxyServer` with the local HTTP endpoint (as `http=` and `https=`) and the
SOCKS endpoint, a bypass list covering loopback, RFC1918 ranges and Windows'
`<local>` marker, `ProxyEnable=1`, followed by an `InternetSetOption` refresh
so running applications pick it up without a re-login.

- The registration is undone on quit, and is withdrawn while TUN mode is up
  (the capture already covers every application; keeping both would only
  double-apply). The **intent** is remembered, so leaving TUN restores the
  registration.
- While 系统代理守卫 is on, the app re-checks the registration every 30 s and
  puts it back if another program changed it. Off by default: while it is on,
  Windows' own proxy switch looks broken because it gets repaired.
- Stale registrations are recognized by content (`ProxyServer` pointing at our
  ports), so a crash that left it behind is cleaned up on the next start.

## TUN mode

TUN mode captures **all** system traffic through a Wintun adapter, so games,
launchers and CLI tools that ignore the system proxy are proxied too.

- **How it works.** tun2socks creates the adapter and forwards every captured
  flow into the engine's own SOCKS5 inbound — the same path a manually
  configured application would take. All routing decisions (rules, GeoIP,
  per-node failover) stay in the engine.
- **Names, not addresses.** Proxied domains are answered from the fake-ip pool
  `198.18.0.0/15`; when the application connects to that address, the engine
  recovers the domain and forwards *the name* to the upstream SOCKS5, which
  resolves it remotely. DNS pollution and IP blocking stop mattering. Domestic
  traffic keeps real addresses and stays direct.
- **Preconditions.** Administrator rights (one UAC prompt from an unelevated
  window), `wintun.dll` next to the exe, and the engine running.
- **No port-53 race.** The DNS hijack answers `:53` queries in-process for
  domains that resolve through the adapter, so no packet ever leaves for the
  wire.

Safety nets, in engagement order:

| Layer | What it covers |
| --- | --- |
| Pipeline probe | An HTTPS fetch through the full capture path right after bring-up, then every 15 s. Three consecutive failures roll the adapter back, returning the machine to its own default route. |
| International probe | Resolves a proxied name (must come back as a fake-ip) and connects to it. Failures are reported, never rolled back: a dead upstream must not take the working domestic path down with it. |
| Bring-up guard | If a bring-up is still unfinished after 120 s (hung wintun call, stuck route transaction), an independent goroutine unwinds the routing state; a bring-up that sees that happen winds itself back instead of finalising on top of it. |
| Lock-free forced teardown | A stuck bring-up holds the state lock; disable, the guard and shutdown all fall back to a path that never takes that lock and re-discovers the adapter from the OS. |
| Auto-retry ladder | A failed bring-up is retried automatically (4 attempts, 5 s doubling backoff, capped), then handed to the chip's manual retry. |
| DNS take-over journal | Replaced resolver lists are journaled to `tun-dns-state.json` before being touched and deleted after being restored; a leftover file from a crash or power loss is replayed by the next bring-up. |
| Process-bound adapter | The adapter is a SwDevice owned by the process, so it — and its routes — disappear with a hard crash even if none of the above ran. |

While TUN is up the system proxy registration is withdrawn (see above); both
can be requested at once, and the app resolves it in TUN's favour.

## Process attribution

The 客户端 column and the 应用 tab resolve connections to the Windows process
that opened them (executable name and icon, read locally — nothing leaves the
machine):

- **System proxy path:** the owner of the loopback client socket is the
  application, directly.
- **TUN path:** the application's socket addressed to the same endpoint whose
  source is the TUN adapter's own address is the owner. Several sockets or
  several helper processes of one executable collapse into one answer; two
  *different* applications on the same endpoint are genuinely ambiguous and
  stay blank instead of guessing.
- Young sessions are re-attributed every 2 s (up to 30 s) when the first
  attempt missed — a throttled downloader's short-lived connections usually
  resolve on the first retry.
- UDP associations carry no per-flow session and are not attributed; the 应用
  tab is therefore TCP-only. Attribution is Windows-only.

## The watchdog pair and updates

`yellowsocks-watchdog.exe` is the guard half of a mutual-restart pair: it
holds its own mutex and restarts the GUI if the app disappears without saying
goodbye (a crash, a kill), taking care not to resurrect it after an intended
exit (`watchdog.stop` marker) or during an update handover.

Updates are done in place:

1. Replace `yellowsocks-gui.exe` (rename the running file, copy the new one)
   and `yellowsocks-watchdog.exe` the same way.
2. Drop an empty `restart.request` next to them. The running app spawns the
   replacement (which waits for the instance claim), writes the stop marker,
   exits; the replacement starts a fresh guard.

`instance.state` records the current instance for the guard. Logs of both
processes land in the same directory; nothing is written to the registry
except the opt-in launch-at-login task and the system proxy keys described
above.

## Data files and logs

Everything lives in the data directory (next to the exe, or
`%APPDATA%\YellowSocks`):

| File | Purpose |
| --- | --- |
| `config.yaml` | The engine configuration, identical in format to the CLI's. |
| `settings.json` | UI/launcher state: system proxy and guard intent, TUN intent, auto-start, launch-at-login, routing mode, language. |
| `stats.json` | The permanent traffic record behind the Statistics page. |
| `yellowsocks-gui_{LEVEL}_{date}.log` | Application log (INFO/WARN/ERROR files, one set per day). |
| `yellowsocks-gui.stderr` | Raw tun2socks output (flow-level debug lines). |
| `tun-dns-state.json` | DNS take-over journal; transient, crash recovery only. |
| `instance.state` | Current instance id for the watchdog pair. |
| `tray-hang-*.log` | Goroutine dump written by the tray watchdog if the tray stops answering. |

## Statistics API and dashboard

The engine serves the same statistics API as the CLI on `api_listen`
(default `127.0.0.1:9090`): JSON status, overview, live connections, traffic,
domains, routes, DNS, errors, nodes and a snapshot endpoint, plus the embedded
HTML dashboard when `api_dashboard` is on. Endpoints, tokens and the pprof
switch are documented in [USAGE.md](USAGE.md#statistics-api). The GUI consumes
the same data through direct Go calls; the API is there for scripts and other
machines on the LAN (listen address and tokens are editable on the Settings
page).

## Known limits

- Process attribution covers TCP only and is Windows-only; UDP relays have no
  per-flow session.
- Connections that existed before TUN came up keep their physical source
  address; attribution still works through the fallback scan, but sockets that
  predate both TUN and the proxy cannot be attributed.
- The statistics tables show active sessions (capped); the lifetime tables on
  the Statistics page are the complete record.
- TUN mode needs the Wintun driver shipped as `wintun.dll`; without
  administrator rights it cannot be enabled.
