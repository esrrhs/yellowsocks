package launch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureWritableWorkDirFromReadOnly(t *testing.T) {
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
