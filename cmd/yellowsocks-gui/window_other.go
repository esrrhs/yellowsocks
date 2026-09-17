//go:build !windows && !darwin

package main

func RunNativeWindow() {
	// Non-windows & non-darwin stub
}

func openControlPanel(url string) {
	openBrowser(url)
}
