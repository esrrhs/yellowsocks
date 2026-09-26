package sysproxy

import "testing"

func TestProxyModeConstants(t *testing.T) {
	if ModeDirect != 0 || ModeGlobal != 1 || ModePAC != 2 {
		t.Fatalf("unexpected mode values: %d %d %d", ModeDirect, ModeGlobal, ModePAC)
	}
}

func TestLinuxNoopProxies(t *testing.T) {
	if err := SetGlobalProxy("127.0.0.1:1080"); err != nil {
		t.Fatalf("SetGlobalProxy: %v", err)
	}
	if err := SetPACProxy("http://127.0.0.1/pac"); err != nil {
		t.Fatalf("SetPACProxy: %v", err)
	}
	if err := ClearSystemProxy(); err != nil {
		t.Fatalf("ClearSystemProxy: %v", err)
	}
}
