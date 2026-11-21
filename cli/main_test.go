package main

import (
	"bytes"
	"net"
	"os"
	"runtime"
	"path/filepath"
	"strings"
	"testing"
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
	if runtime.GOOS == "windows" {
		// loggo keeps the log file open for the process lifetime, so the
		// t.TempDir cleanup of the chdir'ed directory fails on Windows.
		t.Skip("loggo holds the log file open; TempDir cleanup fails on windows")
	}
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
	if runtime.GOOS == "windows" {
		t.Skip("loggo holds the log file open; TempDir cleanup fails on windows")
	}
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

