package state

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAcquireProcessLockRejectsSecondHolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	first, err := AcquireProcessLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	second, err := AcquireProcessLock(dir)
	if err == nil {
		second.Close()
		t.Fatal("second AcquireProcessLock() error = nil")
	}
	if !strings.Contains(err.Error(), "already locked") {
		t.Fatalf("second AcquireProcessLock() error = %v", err)
	}
	if runtime.GOOS != "windows" {
		assertMode(t, dir, 0o700)
		assertMode(t, filepath.Join(dir, "LOCK"), 0o600)
	}
}

func TestProcessLockCanBeAcquiredAfterClose(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireProcessLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := AcquireProcessLock(dir)
	if err != nil {
		t.Fatalf("AcquireProcessLock() after Close() = %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDataDirIsFixedProjectLocalPath(t *testing.T) {
	if DataDir != "./data" {
		t.Fatalf("DataDir = %q, want ./data", DataDir)
	}
	if filepath.IsAbs(DataDir) {
		t.Fatalf("DataDir = %q, want relative path", DataDir)
	}
	if _, err := os.Stat(DataDir); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
