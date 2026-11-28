//go:build windows

package guiapp

import (
	"fmt"
	"net"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The connection table read must return real rows: the whole process
// attribution feature rests on it, and a broken syscall argument order
// used to panic instead of failing loudly.
func TestTCPTableRaw(t *testing.T) {
	var size uint32
	r1, _, callErr := procGetExtTCP.Call(0, uintptr(unsafe.Pointer(&size)), 0, afInet, tcpTableOwnerPIDAll, 0)
	t.Logf("probe: r1=%d size=%d err=%v", r1, size, callErr)
	if size == 0 {
		t.Fatal("size probe returned zero")
	}
}

func TestTCPTableDump(t *testing.T) {
	n := 0
	err := tcpOwnerRows(func(lIP net.IP, lPort, rPort uint16, rIP net.IP, pid uint32) {
		n++
		if n <= 3 {
			fmt.Println("row", lIP, lPort, "->", rIP, rPort, "pid", pid)
		}
	})
	if err != nil {
		t.Fatalf("table read: %v", err)
	}
	if n == 0 {
		t.Fatal("no established rows — the table read is broken")
	}
}

// The system-proxy strategy hinges on resolving a loopback client port to
// its owning process: a real dialed socket must map back to this process.
func TestFindPIDByLocalPortOwnSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	select {
	case c := <-accepted:
		defer c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("accept timed out")
	}
	port := uint16(conn.LocalAddr().(*net.TCPAddr).Port)
	pid, ok := findPIDByLocalPort(port)
	if !ok {
		t.Fatalf("loopback client port %d not found in the TCP table", port)
	}
	if self := windows.GetCurrentProcessId(); pid != self {
		t.Fatalf("owner pid = %d, want %d", pid, self)
	}
}

// A client port with no table row and a domain target must not attribute —
// the fallback remote-address lookup only knows how to find addresses.
func TestFindProcessUnknownClient(t *testing.T) {
	for _, tun := range []bool{true, false} {
		r := newProcessResolver([]string{"127.0.0.1:1080"}, func() bool { return tun })
		if name, icon := r.FindProcess(net.ParseIP("127.0.0.1"), 1, "example.com", 443); name != "" || icon != "" {
			t.Fatalf("tun=%v: unknown client with domain target attributed to %q", tun, name)
		}
	}
}

// The remote-address lookup must answer for this very process: a socket that
// does not ride the TUN address still proves the fallback scan, the
// self-owner rule and the executable-level dedupe are wired correctly.
func TestFindPIDByRemoteTCPOwnSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	select {
	case c := <-accepted:
		defer c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("accept timed out")
	}
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	pid, ok := findPIDByRemoteTCP(net.ParseIP("127.0.0.1"), port)
	if !ok {
		t.Fatalf("dialed endpoint 127.0.0.1:%d not found in the TCP table", port)
	}
	if self := windows.GetCurrentProcessId(); pid != self {
		t.Fatalf("owner pid = %d, want %d", pid, self)
	}
}
