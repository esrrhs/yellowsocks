//go:build windows

package guiapp

import (
	"testing"
	"time"
)

func TestAcquireSingleInstanceAndShowRequest(t *testing.T) {
	t.Cleanup(ReleaseSingleInstance)

	if !AcquireSingleInstance(time.Second) {
		t.Fatal("first acquire failed; another YellowSocks instance may be running")
	}

	shown := make(chan struct{}, 1)
	go WatchShowRequests(func() { shown <- struct{}{} })
	// Give the waiter a moment to block on the event.
	time.Sleep(50 * time.Millisecond)

	if AcquireSingleInstance(0) {
		t.Fatal("second acquire succeeded while the first owner is still holding the mutex")
	}
	select {
	case <-shown:
	case <-time.After(2 * time.Second):
		t.Fatal("owner was not asked to show its window")
	}

	ReleaseSingleInstance()
	if !AcquireSingleInstance(time.Second) {
		t.Fatal("acquire failed after the owner released the mutex")
	}
}
