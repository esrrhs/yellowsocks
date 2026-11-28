//go:build windows

package guiapp

import (
	"fmt"
	"net"
	"unsafe"
	"testing"
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
