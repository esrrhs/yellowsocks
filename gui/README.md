# YellowSocks GUI

Windows / macOS 桌面可视化客户端(类 Clash Verge),基于 [Wails v2](https://wails.io) 构建,
直接内嵌根目录的 `core/` 代理引擎(单进程、单二进制,无 sidecar)。

## 技术栈

- **桌面壳**: Wails v2.17.1(Windows 用 WebView2,macOS 用 WKWebView)
- **前端**: Vue 3 + TypeScript + Vite(无组件库,手写样式)
- **系统托盘**: [energye/systray](https://github.com/energye/systray)
  (Wails v2 官方无托盘,该社区 fork 专为与其他 UI 框架共存设计)
- **引擎**: `github.com/esrrhs/yellowsocks/core`(通过 `replace ../` 引用本仓库根模块)

## 目录结构

```
gui/
├── main.go            # wails.Run 入口,embed 前端产物
├── app.go             # 绑定层: 引擎启停/统计/配置读写,暴露给前端 JS
├── tray.go            # 托盘菜单(显示/启动/停止/退出)
├── app_test.go        # 绑定层单元测试(配置往返、引擎启停)
├── wails.json         # wails 工程配置
├── build/             # 图标、Info.plist、Windows manifest(打包用)
└── frontend/          # Vue3 + TS 前端
    └── src/views/     # Home(状态/统计/流量曲线) Settings(YAML 编辑) About
```

## 开发

前置:Go 1.27+、Node 20+、Wails CLI(`go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.1`)。

```bash
cd gui
wails dev          # 热重载开发(前端改动即时生效)
wails build -webview2 embed   # 生产构建(内嵌 WebView2 bootstrapper)
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
