# YellowSocks GUI

Windows / macOS 桌面可视化客户端(类 Clash Verge),基于 [Wails v2](https://wails.io) 构建,
直接内嵌根目录的 `core/` 代理引擎(单进程、单二进制,无 sidecar)。

## 功能

- **引擎管理**:启动/停止(Home 页按钮、托盘菜单、自动连接选项)
- **上游协议**:SPP 全协议(tcp/udp/rudp/ricmp/kcp/quic/rhttp/rdns)、pingtunnel,以及
  `spp_proto: "socks5"` 直连任意 SOCKS5 服务器(core 引擎新增能力,CLI 同步支持)
- **系统透明代理**:一键把系统流量指向本地代理(Windows 注册表 + WinINET 刷新,
  macOS networksetup);引擎停止/退出自动还原;强杀残留下次启动自动清理
- **设置页**:覆盖 CLI 全部配置项的表单(上游/监听/DNS/路由分流/认证/API/统计)+
  YAML 源码模式,与 CLI 的 config.yaml 完全互通
- **实时统计**:活动连接、上下行速率与总量、30 分钟流量曲线(1s 轮询)
- **系统托盘**:显示主界面/启停/系统代理开关/退出;关闭窗口即最小化到托盘

## 技术栈

- **桌面壳**: Wails v2.17.1(Windows 用 WebView2,macOS 用 WKWebView)
- **前端**: Vue 3 + TypeScript + Vite(无组件库,手写样式)
- **系统托盘**: [energye/systray](https://github.com/energye/systray)
  (Wails v2 官方无托盘,该社区 fork 专为与其他 UI 框架共存设计)
- **引擎**: `github.com/esrrhs/yellowsocks/core`(通过 `replace ../` 引用本仓库根模块)

## 目录结构

```
guiapp/               # 共享绑定层(本目录): app/托盘/系统代理/前端
├── app.go            # 绑定层: 引擎启停/统计/配置读写,暴露给前端 JS
├── tray.go           # 托盘菜单(显示/启动/停止/退出)
├── sysproxy*.go      # 系统透明代理(win registry / mac networksetup)
├── app_test.go       # 绑定层单元测试(配置往返、引擎启停)
└── frontend/         # Vue3 + TS 前端
    └── src/views/    # Home(状态/统计/流量曲线) Settings(YAML 编辑) About

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

GUI 的 `config.yaml` 与 CLI 完全同格式(保存于 `%APPDATA%/YellowSocks/` 或
`~/Library/Application Support/YellowSocks/`),可互相拷贝。首次启动写入默认配置。

注意:引擎总是启动 DNS 服务,默认监听 `127.0.0.1:1053`(53 端口在 macOS 需要 root)。

## 已知限制

- macOS 托盘与 Wails 事件循环的共存依赖 energye/systray 的 darwin 实现,
  CI 只验证编译,运行时行为待真机验证。
- macOS 构建未签名,Gatekeeper 下需右键打开(`xattr -cr`);签名公证后续迭代接入。
- 关闭窗口 = 最小化到托盘,真正退出走托盘菜单"退出"。
