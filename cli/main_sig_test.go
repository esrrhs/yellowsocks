//go:build !windows

package main

import (
	"bytes"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

// End-to-end: start the engine on ephemeral ports and shut it down with
// SIGINT like a real service shutdown.
func TestRunStartsAndStopsOnSignal(t *testing.T) {
	// The SPP client's local SOCKS5 port currently has no CLI flag and
	// defaults to 127.0.0.1:10808; skip when something already holds it.
	if l, err := net.Listen("tcp", "127.0.0.1:10808"); err != nil {
		t.Skipf("default SPP local SOCKS5 port 10808 unavailable: %v", err)
	} else {
		_ = l.Close()
	}

	t.Chdir(t.TempDir()) // keep log output out of the source tree

	dnsAddr := freeUDPPort(t)
	dohAddr := freeTCPPort(t)
	socksAddr := freeTCPPort(t)
	httpAddr := freeTCPPort(t)

	done := make(chan int, 1)
	var out bytes.Buffer
	go func() {
		done <- run([]string{
			"-spp-server", "127.0.0.1:1", // unreachable is fine; traffic is never sent
			"-spp-key", "testkey",
			"-dns", dnsAddr,
			"-doh", dohAddr,
			"-socks5", socksAddr,
			"-http", httpAddr,
			"-loglevel", "error",
		}, &out)
	}()

	// Wait until the inbound SOCKS5 listener is actually up.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", socksAddr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("engine never came up: %s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("send signal: %v", err)
	}

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("shutdown exit code=%d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not shut down after SIGINT")
	}
}
