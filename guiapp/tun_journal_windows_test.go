//go:build windows

package guiapp

import (
	"os"
	"path/filepath"
	"testing"
)

// useTempStateDir points the DNS recovery journal at a throwaway directory for
// the duration of a test. The journal normally lives next to config.yaml.
func useTempStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := tunStateDir
	tunStateDir = func() string { return dir }
	t.Cleanup(func() { tunStateDir = prev })
	return dir
}

// TestDNSStateRoundTrip is the contract the crash recovery depends on: the
// aliases and resolver lists written before the take-over are exactly what a
// later run reads back, including the empty list that means "DHCP-provided".
func TestDNSStateRoundTrip(t *testing.T) {
	useTempStateDir(t)

	saved := []tunStateEntry{
		{Alias: "Ethernet", Servers: []string{"192.168.1.1", "192.168.0.1"}},
		{Alias: "Wi-Fi"}, // no explicit servers: DHCP handed them out
	}
	saveDNSState(saved)

	if _, err := os.Stat(tunStatePath()); err != nil {
		t.Fatalf("journal was not written: %v", err)
	}
	got, ok := loadDNSState()
	if !ok {
		t.Fatal("loadDNSState reported no journal right after saveDNSState")
	}
	if len(got) != len(saved) {
		t.Fatalf("got %d entries, want %d", len(got), len(saved))
	}
	if got[0].Alias != "Ethernet" || len(got[0].Servers) != 2 ||
		got[0].Servers[0] != "192.168.1.1" || got[0].Servers[1] != "192.168.0.1" {
		t.Fatalf("static resolvers did not survive the round trip: %+v", got[0])
	}
	if got[1].Alias != "Wi-Fi" || len(got[1].Servers) != 0 {
		t.Fatalf("a DHCP interface must come back with an empty list: %+v", got[1])
	}
}

// TestDNSStateAbsent: no journal means nothing to repair, and nothing is
// created by looking for it.
func TestDNSStateAbsent(t *testing.T) {
	useTempStateDir(t)

	if _, ok := loadDNSState(); ok {
		t.Fatal("loadDNSState invented a journal")
	}
	if _, err := os.Stat(tunStatePath()); !os.IsNotExist(err) {
		t.Fatalf("loadDNSState created %s", tunStatePath())
	}
}

// TestDNSStateCorruptIsDropped: a half-written journal can never be applied,
// so it must be removed instead of warning on every later bring-up.
func TestDNSStateCorruptIsDropped(t *testing.T) {
	useTempStateDir(t)

	if err := os.WriteFile(tunStatePath(), []byte(`{"dns":[{"alias":"Ether`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadDNSState(); ok {
		t.Fatal("a truncated journal was accepted")
	}
	if _, err := os.Stat(tunStatePath()); !os.IsNotExist(err) {
		t.Fatal("a truncated journal was kept")
	}
}

// TestDNSStateEmptyIsDropped: a journal with no entries restores nothing, so
// it is stale by definition.
func TestDNSStateEmptyIsDropped(t *testing.T) {
	useTempStateDir(t)

	if err := os.WriteFile(tunStatePath(), []byte(`{"dns":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadDNSState(); ok {
		t.Fatal("an empty journal was accepted")
	}
	if _, err := os.Stat(tunStatePath()); !os.IsNotExist(err) {
		t.Fatal("an empty journal was kept")
	}
}

// TestClearDNSStateIsIdempotent: teardown and shutdown can both reach it.
func TestClearDNSStateIsIdempotent(t *testing.T) {
	dir := useTempStateDir(t)

	saveDNSState([]tunStateEntry{{Alias: "Ethernet", Servers: []string{"192.168.1.1"}}})
	clearDNSState()
	clearDNSState()
	if _, err := os.Stat(filepath.Join(dir, tunStateName)); !os.IsNotExist(err) {
		t.Fatal("clearDNSState left the journal behind")
	}
}

// TestSaveDNSStateSkipsEmptyList: with no adapters touched there is nothing to
// remember, and writing an empty journal would only look like a crash.
func TestSaveDNSStateSkipsEmptyList(t *testing.T) {
	useTempStateDir(t)

	saveDNSState(nil)
	if _, err := os.Stat(tunStatePath()); !os.IsNotExist(err) {
		t.Fatal("saveDNSState wrote a journal for an empty take-over")
	}
}
