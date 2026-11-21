# YellowSocks GUI

Windows / macOS 桌面可视化客户端(类似 Clash Verge),基于 [Wails](https://wails.io) 或 Tauri 构建,
复用根目录的 `core/` 引擎(通过 Wails 绑定或以 `cli/` 二进制作为 sidecar 进程)。

状态:规划中,尚未开始。

## 结构规划

```
gui/
├── frontend/    # 前端 UI (Vue/React + TS)
└── ...          # 桌面壳工程
```
