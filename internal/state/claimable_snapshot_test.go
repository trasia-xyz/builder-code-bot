package state

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"builder-code-bot/internal/funding"
)

func TestStoreSavesAndLoadsImmutableClaimableSnapshot(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	snapshot := testClaimableSnapshot()
	if err := store.SaveClaimableSnapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadClaimableSnapshot(context.Background(), snapshot.UTCDate)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || !loaded.CapturedAt.Equal(snapshot.CapturedAt) ||
		len(loaded.Builders) != 2 || loaded.Builders[0].ClaimableUSDC != "1.25" ||
		!loaded.Builders[1].QueryFailed {
		t.Fatalf("loaded snapshot = %#v", loaded)
	}
	replacement := snapshot
	replacement.Builders = append([]funding.ClaimableSnapshotBuilder(nil), snapshot.Builders...)
	replacement.Builders[0].ClaimableUSDC = "9.25"
	if err := store.SaveClaimableSnapshot(context.Background(), replacement); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second SaveClaimableSnapshot() error = %v", err)
	}
	reloaded, err := store.LoadClaimableSnapshot(context.Background(), snapshot.UTCDate)
	if err != nil || reloaded == nil || reloaded.Builders[0].ClaimableUSDC != "1.25" {
		t.Fatalf("immutable snapshot after second save = %#v, %v", reloaded, err)
	}

	if runtime.GOOS != "windows" {
		assertMode(t, dir, 0o700)
		assertMode(t, filepath.Join(dir, claimableSnapshotDirname), 0o700)
		assertMode(
			t,
			filepath.Join(dir, claimableSnapshotDirname, snapshot.UTCDate+".json"),
			0o600,
		)
	}
}

func TestStoreRejectsTamperedClaimableSnapshot(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	snapshot := testClaimableSnapshot()
	if err := store.SaveClaimableSnapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, claimableSnapshotDirname, snapshot.UTCDate+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"claimable_usdc":"1.25"`, `"claimable_usdc":"9.25"`, 1)
	if tampered == string(data) {
		t.Fatal("saved snapshot did not contain the expected amount")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadClaimableSnapshot(context.Background(), snapshot.UTCDate)
	if err == nil || loaded != nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("LoadClaimableSnapshot() = %#v, %v; want checksum error", loaded, err)
	}
}

func TestClaimableSnapshotStoreHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := NewStore(t.TempDir())
	if err := store.SaveClaimableSnapshot(ctx, testClaimableSnapshot()); err != context.Canceled {
		t.Fatalf("SaveClaimableSnapshot() error = %v, want context.Canceled", err)
	}
	if snapshot, err := store.LoadClaimableSnapshot(ctx, "2026-07-12"); err != context.Canceled ||
		snapshot != nil {
		t.Fatalf("LoadClaimableSnapshot() = %#v, %v; want context.Canceled", snapshot, err)
	}
}

func testClaimableSnapshot() funding.ClaimableSnapshot {
	return funding.ClaimableSnapshot{
		UTCDate:    "2026-07-12",
		CapturedAt: time.Date(2026, 7, 12, 0, 0, 1, 0, time.UTC),
		Builders: []funding.ClaimableSnapshotBuilder{
			{Address: "0xbuilder-1", ClaimableUSDC: "1.25"},
			{Address: "0xbuilder-2", QueryFailed: true},
		},
	}
}
