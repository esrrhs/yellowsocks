//go:build windows

package main

import (
	"fmt"
	"github.com/jchv/go-webview2"
)

// RunNativeWindow 启动现代 Windows 原生 WebView2 桌面窗口 (类似于 Clash Verge / Tauri)
func RunNativeWindow() {
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "YellowSocks - Control Panel",
			Width:  960,
			Height: 680,
			IconId: 2, // 默认应用图标
			Center: true,
		},
	})
	if w == nil {
		fmt.Println("Failed to create WebView2 window. Ensure WebView2 Runtime is installed.")
		return
	}
	defer w.Destroy()

	// 绑定 Go 原生函数到 JS，实现桌面 UI 与内核无缝交互
	_ = w.Bind("toggleTUN", func() (bool, error) {
		if engine == nil {
			err := startProxyEngine()
			return err == nil, err
		}
		stopProxyEngine()
		return false, nil
	})

	_ = w.Bind("toggleFakeIP", func() bool {
		appCfg.EnableFakeIP = !appCfg.EnableFakeIP
		return appCfg.EnableFakeIP
	})

	// 加载内嵌的现代仪表盘页面
	w.SetHtml(string(embeddedWebDashboard))

	// 运行 Windows UI 消息主循环
	w.Run()
}
