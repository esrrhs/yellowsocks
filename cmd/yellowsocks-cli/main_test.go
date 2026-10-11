package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func freeUDPPort(t *testing.T) string {
	t.Helper()
	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.LocalAddr().String()
	_ = l.Close()
	return addr
}

func freeTCPPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestRunVersionFlags(t *testing.T) {
	for _, arg := range []string{"-v", "-version"} {
		var out bytes.Buffer
		code := run([]string{arg}, &out)
		if code != 0 {
			t.Fatalf("%s exit code=%d", arg, code)
		}
		if !strings.Contains(strings.ToLower(out.String()), "yellowsocks") {
			t.Fatalf("%s output missing binary name: %q", arg, out.String())
		}
	}
}

func TestRunUnknownFlag(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"-definitely-not-a-flag"}, &out); code != 2 {
		t.Fatalf("unknown flag exit code=%d, want 2", code)
	}
}

func TestRunBadConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("garbage-not-config"), 0644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := run([]string{"-config", path}, &out)
	if code != 1 {
		t.Fatalf("bad config exit code=%d, want 1", code)
	}
	if !strings.Contains(out.String(), "Error loading config file") {
		t.Fatalf("output missing error message: %q", out.String())
	}
}

func TestRunMissingConfigFile(t *testing.T) {
	var out bytes.Buffer
	code := run([]string{"-config", filepath.Join(t.TempDir(), "nope.yaml")}, &out)
	if code != 1 {
		t.Fatalf("missing config exit code=%d, want 1", code)
	}
}

func TestRunNoServerPrintsUsage(t *testing.T) {
	t.Chdir(t.TempDir()) // run() opens a log file in the working directory
	var out bytes.Buffer
	code := run([]string{"-loglevel", "error"}, &out)
	if code != 0 {
		t.Fatalf("usage path exit code=%d, want 0", code)
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("output missing usage: %q", out.String())
	}
}

// A config file without an SPP server also lands on the usage path, proving
// file config is loaded and merged before the guard runs.
func TestRunConfigWithoutServerShowsUsage(t *testing.T) {
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("loglevel: error\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run([]string{"-config", path}, &out); code != 0 ||
		!strings.Contains(out.String(), "Usage:") {
		t.Fatalf("code=%d out=%q", code, out.String())
	}
}

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
