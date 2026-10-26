package launch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureWritableWorkDirFromReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("skipping read-only directory test under root (root bypasses mode 0555)")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	ro := t.TempDir()
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	if err := os.Chdir(ro); err != nil {
		t.Fatal(err)
	}

	if !EnsureWritableWorkDir() {
		t.Fatal("expected a writable fallback directory")
	}
	got, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got == ro {
		t.Fatalf("still in read-only dir %s", got)
	}
	probe := filepath.Join(got, ".yellowsocks-write-test")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	_ = os.Remove(probe)
}

// A writable working directory is kept as-is: the fast path must report
// success without chdir-ing anywhere else.
func TestEnsureWritableWorkDirKeepsWritableCwd(t *testing.T) {
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	if !EnsureWritableWorkDir() {
		t.Fatal("writable cwd must be accepted")
	}
	got, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatal(err)
	}
	if gotResolved != want {
		t.Fatalf("cwd moved from %s to %s", tmp, got)
	}
}

// Every fallback candidate fails because its YellowSocks subdirectory already
// exists but is not writable, so the function must give up and return false.
func TestEnsureWritableWorkDirAllFallbacksUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("skipping read-only directory test under root (root bypasses mode 0555)")
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	home := t.TempDir()
	// Pre-create each fallback the function derives from $HOME as a read-only
	// directory: MkdirAll then succeeds (the dir exists) but the write probe
	// fails.
	for _, sub := range []string{
		filepath.Join("Library", "Application Support", "YellowSocks"),
		filepath.Join("Library", "Caches", "YellowSocks"),
		filepath.Join("Documents", "YellowSocks"),
	} {
		dir := filepath.Join(home, sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	}

	tmpBase := t.TempDir()
	tmpCandidate := filepath.Join(tmpBase, "YellowSocks")
	if err := os.MkdirAll(tmpCandidate, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmpCandidate, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmpCandidate, 0o755) })

	// Create the read-only starting directory before TMPDIR is overridden,
	// because t.TempDir itself places new directories under TMPDIR.
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })

	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", tmpBase)

	if err := os.Chdir(ro); err != nil {
		t.Fatal(err)
	}

	if EnsureWritableWorkDir() {
		t.Fatalf("expected false when every fallback directory is unwritable (home=%s tmp=%s)", home, tmpBase)
	}
}

// Every fallback candidate fails already at MkdirAll because its parent is
// read-only, so the function must give up and return false.
func TestEnsureWritableWorkDirFallbacksMkdirFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("skipping read-only directory test under root (root bypasses mode 0555)")
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	roHome := t.TempDir()
	if err := os.Chmod(roHome, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roHome, 0o755) })

	roTmp := t.TempDir()
	if err := os.Chmod(roTmp, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roTmp, 0o755) })

	// Create the read-only starting directory before TMPDIR is overridden,
	// because t.TempDir itself places new directories under TMPDIR.
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })

	t.Setenv("HOME", roHome)
	t.Setenv("TMPDIR", roTmp)

	if err := os.Chdir(ro); err != nil {
		t.Fatal(err)
	}

	if EnsureWritableWorkDir() {
		t.Fatalf("expected false when every fallback parent directory is read-only (home=%s tmp=%s)", roHome, roTmp)
	}
}
