//go:build !windows

package main

import (
	"fmt"
	"os"
)

// The mutual-restart guard is built on Windows named mutexes; the binary is
// only shipped on Windows.
func run() int {
	fmt.Fprintln(os.Stderr, "yellowsocks-watchdog only supports Windows")
	return 1
}
