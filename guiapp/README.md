# YellowSocks GUI

Windows / macOS 桌面可视化客户端(类 Clash Verge),基于 [Wails v2](https://wails.io) 构建,
直接内嵌根目录的 `core/` 代理引擎(单进程、单二进制,无 sidecar)。

## 功能

- **引擎管理**:启动/停止(Home 页按钮、托盘菜单、自动连接选项)
- **上游协议**:SPP 全协议(tcp/udp/rudp/ricmp/kcp/quic/rhttp/rdns)、pingtunnel,以及
  `spp_proto: "socks5"` 直连任意 SOCKS5 服务器(core 引擎新增能力,CLI 同步支持)
- **系统透明代理**:一键把系统流量指向本地代理(Windows 注册表 + WinINET 刷新,
  macOS networksetup);引擎停止/退出自动还原;强杀残留下次启动自动清理
- **开机启动**(Windows):注册 HKCU Run 键,登录后以 `--hidden` 参数启动到托盘;
  配合"自动连接"+"系统代理"实现开机自动透明代理。exe 移动后下次启动自动修正注册路径
- **TUN 模式**(Windows):Clash Verge 式全局透明代理 — wintun 虚拟网卡 +
  tun2socks v2 (gVisor) 用户态协议栈,接管全部系统流量,Firefox 等不走系统代理
  的应用零配置直连;53 端口 DNS 劫持到引擎智能分流 DNS;分流与统计与系统代理
  模式完全一致(国内直连/国外代理/永久累计)。开启需一次 UAC 提权重启
  (实现细节见下文「TUN 模式实现细节」)
- **绿色单文件**(Windows):把 `yellowsocks-gui.exe` 单独拷到任意可写目录即可运行,
  config.yaml / settings.json / stats.json / 日志全部生成在 exe 旁边;目录只读
  (如 Program Files、签名后的 .app 包)时自动退回用户目录,并会把旧数据一次性
  迁移过来(不覆盖已有文件)
- **设置页**:覆盖 CLI 全部配置项的表单(上游/监听/DNS/路由分流/认证/API/统计)+
  YAML 源码模式,与 CLI 的 config.yaml 完全互通
- **实时统计**:活动连接、上下行速率与总量、30 分钟流量曲线(1s 轮询)
- **系统托盘**:显示主界面/启停/系统代理开关/退出;关闭窗口即最小化到托盘

### TUN 模式实现细节(Windows,`tun_windows.go`)

- 适配器 `YellowSocks`(固定 GUID,持久复用),地址 `198.18.0.1/30` +
  `fdfe:dcba:9876::1/126`,MTU 1500;wintun.dll 内嵌在 exe 里,启用时释放到 exe 旁
- 路由:`0.0.0.0/1 + 128.0.0.0/1`(IPv6 为 `::/1 + 8000::/1`)走 TUN,下一跳为
  TUN 自身地址;上游服务器 IP 加 `/32` 物理网关例外;适配器 DNS 设为**对端地址**
  `198.18.0.2`(本机地址的包不会进网卡,劫持会失效 — 踩过的坑)
- 防回环:`core/netdial` 把引擎全部直连拨号(TCP/UDP、直连 DNS、DoH、上游)
  用 `IP_UNICAST_IF` 绑定到物理网卡
- 流量管线:TUN → gVisor → 自定义 Handler → 53 端口劫持到引擎 DNS 监听,
  其余 TCP 走引擎 SOCKS5 入站 CONNECT、UDP 走 UDP ASSOCIATE → 引擎照常分流记账
- 提权:整体提权重启(ShellExecute runas + 文件握手确认存活,旧实例 30 秒内
  未确认新实例则放弃);`EnableFrontendEvents()` 区分 GUI 与无 Wails 环境
- 已知限制:Firefox DoH 旁路劫持时按 GeoIP 兜底;开机自启 + TUN 登录时会弹 UAC;
  强杀进程会在路由表留下 `/1` 残留,下次启用 TUN 时自动清空;macOS 未接入

## 技术栈

- **桌面壳**: Wails v2.17.1(Windows 用 WebView2,macOS 用 WKWebView)
- **前端**: Vue 3 + TypeScript + Vite(无组件库,手写样式)
- **系统托盘**: [energye/systray](https://github.com/energye/systray)
  (Wails v2 官方无托盘,该社区 fork 专为与其他 UI 框架共存设计)
- **引擎**: `github.com/esrrhs/yellowsocks/core`(通过 `replace ../` 引用本仓库根模块)

## 目录结构

```
guiapp/               # 共享绑定层(本目录): app/托盘/系统代理/开机启动/绿色模式/前端
├── app.go            # 绑定层: 引擎启停/统计/配置读写,暴露给前端 JS
├── tray.go           # 托盘菜单(显示/启动/停止/退出)
├── sysproxy*.go      # 系统透明代理(win registry / mac networksetup)
├── autostart_*.go    # 开机启动(win Run 键;其他平台暂不支持,返回错误)
├── portable.go       # 绿色模式: 数据文件跟随 exe + 启动引导 Boot()
├── app_test.go       # 绑定层单元测试(配置往返、引擎启停、统计持久化)
└── frontend/         # Vue3 + TS 前端
    └── src/views/    # Home(状态/统计/累计流量/流量曲线) Settings(YAML 编辑) About

windows/              # Windows 壳工程: main.go + wails.json + 图标/manifest
macos/                # macOS 壳工程: main.go + wails.json + Info.plist
cli/                  # CLI
```

前端以 windows/frontend 为源,改完跑 `bash scripts/sync-frontend.sh` 同步到 macos/frontend。

## 开发

前置:Go 1.27+、Node 20+、Wails CLI(`go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.1`)。

```bash
cd windows    # 或 macos
wails dev                      # 热重载开发(前端改动即时生效)
wails build -webview2 embed    # 生产构建(Windows 内嵌 WebView2 bootstrapper)
```

产物在 `gui/build/bin/`:Windows 为 `yellowsocks-gui.exe`,macOS 为 `yellowsocks-gui.app`。

## 配置

GUI 的 `config.yaml` 与 CLI 完全同格式,可互相拷贝。数据文件位置分两种模式:

- **绿色模式**(exe 目录可写,Windows 单文件分发的默认情形):全部保存在 exe 旁边;
- **回退模式**(exe 目录只读):保存在 `%APPDATA%/YellowSocks/` 或
  `~/Library/Application Support/YellowSocks/`。

首次启动写入默认配置;流量统计默认写入同目录 `stats.json`(配置里的
`stats_file` 可覆盖)。安装包升级到绿色模式时,旧的用户目录数据会被自动迁移。

注意:引擎总是启动 DNS 服务,默认监听 `127.0.0.1:1053`(53 端口在 macOS 需要 root)。

## 已知限制

- macOS 托盘与 Wails 事件循环的共存依赖 energye/systray 的 darwin 实现,
  CI 只验证编译,运行时行为待真机验证。
- macOS 构建未签名,Gatekeeper 下需右键打开(`xattr -cr`);签名公证后续迭代接入。
- 关闭窗口 = 最小化到托盘,真正退出走托盘菜单"退出"。
- 开机启动目前仅支持 Windows;macOS 走 LaunchAgent 待后续接入。
- `wails build`/`wails generate module` 生成的绑定工具以 `-tags bindings` 编译,
  guiapp 的 `Boot()` 在该模式下是空操作(否则绑定工具会被绿色模式的 chdir 带离
  项目目录,绑定生成会静默失败)。
