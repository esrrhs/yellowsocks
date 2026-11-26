package guiapp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/esrrhs/yellowsocks/core/stats"
)

// TestPortableDataDir checks that the portable layout resolves to the exe
// directory when it is writable (the normal case for `go test` binaries and
// green deployments) and that the data files are named inside it.
func TestPortableDataDir(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path in this environment")
	}
	if !dirWritable(filepath.Dir(exe)) {
		t.Skip("test binary directory is not writable; portable mode not exercised")
	}
	dir := DataDir()
	if dir != filepath.Dir(exe) {
		t.Fatalf("DataDir = %s, want exe directory %s", dir, filepath.Dir(exe))
	}
	cp, err := configPath()
	if err != nil {
		t.Fatalf("configPath: %v", err)
	}
	if cp != filepath.Join(dir, configName) {
		t.Fatalf("configPath = %s, want %s", cp, filepath.Join(dir, configName))
	}
	if sp, err := settingsPath(); err != nil || sp != filepath.Join(dir, settingsName) {
		t.Fatalf("settingsPath = %s (err %v)", sp, err)
	}
	if statsPath() != filepath.Join(dir, statsName) {
		t.Fatalf("statsPath = %s, want %s", statsPath(), filepath.Join(dir, statsName))
	}
}

// TestAdoptUserData verifies the one-time migration: data files that still
// live in the per-user directory are copied into a fresh portable directory
// without overwriting anything already there.
func TestAdoptUserData(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path in this environment")
	}
	portable := filepath.Dir(exe)
	if !dirWritable(portable) || portable == userConfigDataDir() {
		t.Skip("portable mode not active in this environment")
	}
	useTempConfigDir(t)

	// Earlier tests (or previous runs) may have left data files in the
	// portable directory; clear the ones this test controls so the adoption
	// actually has something to do. Existing files must not be overwritten
	// by AdoptUserData, which is exactly what the settings.json step checks.
	for _, name := range []string{configName, statsName} {
		if err := os.Remove(filepath.Join(portable, name)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}

	// Simulate an old installation with config in the per-user directory.
	oldCfg := "spp_server: \"1.2.3.4:8888\"\n"
	if err := os.MkdirAll(userConfigDataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userConfigDataDir(), configName), []byte(oldCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file already in the portable dir must survive untouched.
	keep := "keep: me\n"
	if err := os.WriteFile(filepath.Join(portable, settingsName), []byte(keep), 0o644); err != nil {
		t.Fatal(err)
	}

	AdoptUserData()

	migrated, err := os.ReadFile(filepath.Join(portable, configName))
	if err != nil {
		t.Fatalf("config.yaml not adopted: %v", err)
	}
	if string(migrated) != oldCfg {
		t.Fatalf("adopted config = %q, want %q", migrated, oldCfg)
	}
	kept, err := os.ReadFile(filepath.Join(portable, settingsName))
	if err != nil {
		t.Fatal(err)
	}
	if string(kept) != keep {
		t.Fatalf("existing portable file was overwritten: %q", kept)
	}
}

// TestResetLifetimeStatsStoppedEngine checks that clearing the permanent
// record while the engine is down rewrites the snapshot file, so the old
// totals do not come back on the next launch.
func TestResetLifetimeStatsStoppedEngine(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path in this environment")
	}
	if !dirWritable(filepath.Dir(exe)) || DataDir() != filepath.Dir(exe) {
		t.Skip("portable mode not active in this environment")
	}

	// Seed a snapshot with lifetime totals, then clear them through the
	// binding used by the UI.
	seed := stats.New(stats.Config{File: statsPath(), SaveInterval: 0})
	s := seed.Begin(stats.ProtoHTTP, "", "reset-test.com", 80, stats.DecisionProxy, "")
	s.AddDownload(4096)
	s.End(nil)
	seed.Close()

	a := NewApp()
	a.loadLifetimeStats()
	if got := a.GetLifetimeStats().Totals.Download; got != 4096 {
		t.Fatalf("seeded lifetime download = %d, want 4096", got)
	}

	if err := a.ResetLifetimeStats(); err != nil {
		t.Fatalf("ResetLifetimeStats: %v", err)
	}

	all, _, _ := stats.LifetimeTotals(statsPath())
	if all.Total() != 0 {
		t.Fatalf("lifetime in %s after reset = %+v, want zero", statsPath(), all)
	}
	if got := a.GetLifetimeStats().Totals.Total(); got != 0 {
		t.Fatalf("cached lifetime after reset = %d, want 0", got)
	}
}

func TestLaunchFlagsFromArgs(t *testing.T) {
	orig := append([]string(nil), os.Args...)
	t.Cleanup(func() { os.Args = orig })

	os.Args = []string{"yellowsocks-gui"}
	if StartHiddenRequested() || ElevatedRequested() || ElevatedDirect() || Restarted() {
		t.Fatal("flags reported set with no arguments")
	}

	os.Args = []string{"yellowsocks-gui", "--hidden", "--elevated"}
	if !StartHiddenRequested() || !ElevatedRequested() {
		t.Fatal("did not see --hidden/--elevated")
	}
	if ElevatedDirect() || Restarted() {
		t.Fatal("unrelated flags were reported set")
	}

	os.Args = []string{"yellowsocks-gui", "--elevated-direct"}
	if !ElevatedDirect() || ElevatedRequested() {
		t.Fatal("--elevated-direct must not also count as --elevated")
	}

	os.Args = []string{"yellowsocks-gui", "--restarted"}
	if !Restarted() {
		t.Fatal("did not see --restarted")
	}
}

func TestRestartFlagHelpers(t *testing.T) {
	if !isHiddenFlag("--hidden") || !isHiddenFlag("/hidden") || isHiddenFlag("--elevated") {
		t.Fatal("isHiddenFlag mismatch")
	}
	if !isRestartedFlag("--restarted") || !isRestartedFlag("/restarted") || isRestartedFlag("--hidden") {
		t.Fatal("isRestartedFlag mismatch")
	}
}

func TestElevatedHandshakeRoundTrip(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path in this environment")
	}
	if !dirWritable(filepath.Dir(exe)) || DataDir() != filepath.Dir(exe) {
		t.Skip("portable mode not active in this environment")
	}

	ClearElevatedHandshake()
	t.Cleanup(ClearElevatedHandshake)
	if WaitElevatedHandshake(200 * time.Millisecond) {
		t.Fatal("handshake succeeded with no marker file")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(50 * time.Millisecond)
		WriteElevatedHandshake()
	}()
	if !WaitElevatedHandshake(2 * time.Second) {
		t.Fatal("handshake was not observed")
	}
	<-done
	if _, err := os.Stat(handshakePath()); !os.IsNotExist(err) {
		t.Fatal("successful wait left the marker file behind")
	}
}

func TestRestartRequestPathIsInDataDir(t *testing.T) {
	got := RestartRequestPath()
	want := filepath.Join(DataDir(), RestartRequestName)
	if got != want {
		t.Fatalf("RestartRequestPath = %s, want %s", got, want)
	}
}
