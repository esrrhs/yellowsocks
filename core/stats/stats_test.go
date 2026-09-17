package stats

import (
	"fmt"
	"sync"
	"testing"
)

func TestStatsAddTrafficAndSnapshot(t *testing.T) {
	mgr := NewManager()
	defer mgr.Close()

	mgr.AddTraffic(1024, 2048)
	mgr.AddTraffic(512, 1024)

	snap := mgr.GetSnapshot()
	if snap.TotalUpload != 1536 {
		t.Errorf("expected total upload 1536, got %d", snap.TotalUpload)
	}
	if snap.TotalDownload != 3072 {
		t.Errorf("expected total download 3072, got %d", snap.TotalDownload)
	}
}

func TestStatsConnectionLifecycle(t *testing.T) {
	mgr := NewManager()
	defer mgr.Close()

	connID := "conn-1001"
	mgr.TrackConnection(connID, "chrome.exe", "127.0.0.1:54321", "1.1.1.1:443", "cloudflare.com", "Proxy")

	snap := mgr.GetSnapshot()
	if len(snap.ActiveConns) != 1 {
		t.Fatalf("expected 1 active connection, got %d", len(snap.ActiveConns))
	}
	conn := snap.ActiveConns[0]
	if conn.ID != connID || conn.Process != "chrome.exe" || conn.Rule != "Proxy" {
		t.Errorf("unexpected connection record: %+v", conn)
	}

	// Update traffic for this connection
	mgr.UpdateConnectionTraffic(connID, 500, 1500)
	snap = mgr.GetSnapshot()
	if len(snap.ActiveConns) != 1 || snap.ActiveConns[0].Upload != 500 || snap.ActiveConns[0].Download != 1500 {
		t.Errorf("expected updated traffic, got %+v", snap.ActiveConns[0])
	}

	// Remove connection
	mgr.RemoveConnection(connID)
	snap = mgr.GetSnapshot()
	if len(snap.ActiveConns) != 0 {
		t.Errorf("expected 0 active connections after removal, got %d", len(snap.ActiveConns))
	}
}

func TestStatsLogBuffer(t *testing.T) {
	mgr := NewManager()
	defer mgr.Close()

	for i := 0; i < 250; i++ {
		mgr.AddLog(fmt.Sprintf("log line %d", i))
	}

	snap := mgr.GetSnapshot()
	if len(snap.Logs) > 200 {
		t.Errorf("expected max 200 logs, got %d", len(snap.Logs))
	}
}

func TestStatsConcurrency(t *testing.T) {
	mgr := NewManager()
	defer mgr.Close()

	var wg sync.WaitGroup
	workers := 20
	iterations := 100

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				id := fmt.Sprintf("conn-%d-%d", workerID, i)
				mgr.TrackConnection(id, "proc", "src", "target", "domain", "Direct")
				mgr.UpdateConnectionTraffic(id, 10, 20)
				_ = mgr.GetSnapshot()
				mgr.RemoveConnection(id)
			}
		}(w)
	}
	wg.Wait()
}
