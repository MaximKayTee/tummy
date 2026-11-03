package lock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestAcquireRelease(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "locks")
	a, err := Acquire(dir, "sales")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir, "sales"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second holder in the same process: %v", err)
	}
	if Free(dir, "sales") {
		t.Fatal("held lock reported free")
	}
	other, err := Acquire(dir, "other")
	if err != nil {
		t.Fatal(err)
	}
	other.Release()
	if err := a.Release(); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(); err != nil {
		t.Fatal("second release should be a no-op")
	}
	if !Free(dir, "sales") {
		t.Fatal("released lock not free")
	}
	var nilLock *Lock
	if err := nilLock.Release(); err != nil {
		t.Fatal(err)
	}
	di, _ := os.Stat(dir)
	fi, _ := os.Stat(filepath.Join(dir, "sales.lock"))
	if di.Mode().Perm() != 0o700 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("modes: dir %v file %v", di.Mode(), fi.Mode())
	}
}

// Another process holding the lock makes Acquire fail; its exit frees the lock.
func TestAcquireAcrossProcesses(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock(1) not installed")
	}
	dir := t.TempDir()
	cmd := exec.Command("flock", filepath.Join(dir, "x.lock"), "sh", "-c", "echo held; read _")
	stdin, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := out.Read(buf); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir, "x"); !errors.Is(err, ErrBusy) {
		t.Fatalf("held by another process: %v", err)
	}
	stdin.Close()
	cmd.Wait()
	l, err := Acquire(dir, "x")
	if err != nil {
		t.Fatalf("after the holder exited: %v", err)
	}
	l.Release()
}
