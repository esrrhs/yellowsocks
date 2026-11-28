//go:build windows

package main

import (
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// mutexHeld is the watchdog's liveness probe for the app; a false positive
// would restart a running app, a false negative would leave a dead one down.
func TestMutexHeldProbe(t *testing.T) {
	name := fmt.Sprintf(`Local\YellowSocksWatchdogTestMutex%d`, os.Getpid())
	if mutexHeld(name) {
		t.Fatal("an unclaimed mutex was reported as held")
	}
	handle, claimed, cerr := claimMutex(name)
	if !claimed || handle == 0 {
		t.Fatalf("claimMutex: claimed=%v err=%v", claimed, cerr)
	}
	defer windows.CloseHandle(handle)
	if !mutexHeld(name) {
		t.Fatal("a claimed mutex was reported as free")
	}
}

// The claim must stay held: releasing it early is what let the app stack up
// one watchdog every ensure tick instead of keeping the first one.
func TestClaimMutexHoldsTheName(t *testing.T) {
	name := fmt.Sprintf(`Local\YellowSocksWatchdogTestClaim%d`, os.Getpid())
	handle, claimed, cerr := claimMutex(name)
	if !claimed || handle == 0 {
		t.Fatalf("first claim failed: claimed=%v err=%v", claimed, cerr)
	}
	if second, claimedAgain, _ := claimMutex(name); claimedAgain || second != 0 {
		t.Fatal("a second claim succeeded while the first handle is open")
	}
	windows.CloseHandle(handle)
	if third, claimedAgain, cerr := claimMutex(name); !claimedAgain || third == 0 {
		t.Fatalf("claim failed after release: claimed=%v err=%v", claimedAgain, cerr)
	} else {
		windows.CloseHandle(third)
	}
}
