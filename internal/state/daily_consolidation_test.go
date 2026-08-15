package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"builder-code-bot/internal/funding"
)

func TestStoreSavesCapturedThenCompletedDailyConsolidation(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	record := testDailyConsolidation()
	captured := record
	captured.Builders = append([]funding.DailyConsolidationBuilder(nil), record.Builders...)
	captured.Status = funding.DailyConsolidationClaimablesCaptured
	captured.CompletedAt = time.Time{}
	for index := range captured.Builders {
		captured.Builders[index].ClaimAttempts = 0
		captured.Builders[index].ClaimUncertain = false
		captured.Builders[index].ClaimStatus = funding.ReportStepPending
		captured.Builders[index].SweepStatus = funding.ReportStepPending
	}
	if err := store.SaveDailyConsolidation(context.Background(), captured); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDailyConsolidation(context.Background(), record); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadDailyConsolidation(context.Background(), record.UTCDate)
	if loaded == nil || !loaded.CompletedAt.Equal(record.CompletedAt) ||
		err != nil || loaded.Status != funding.DailyConsolidationCompleted ||
		len(loaded.Builders) != 2 || loaded.Builders[0].ClaimableUSDC != "1.25" ||
		!loaded.Builders[1].ClaimUncertain {
		t.Fatalf("loaded daily consolidation = %#v", loaded)
	}

	if runtime.GOOS != "windows" {
		assertMode(t, dir, 0o700)
		assertMode(t, filepath.Join(dir, dailyConsolidationDirname), 0o700)
		assertMode(
			t,
			filepath.Join(dir, dailyConsolidationDirname, record.UTCDate+".json"),
			0o600,
		)
	}
}

func TestStoreRejectsTamperedDailyConsolidation(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	record := testDailyConsolidation()
	if err := store.SaveDailyConsolidation(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, dailyConsolidationDirname, record.UTCDate+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"claimable_usdc": "1.25"`, `"claimable_usdc": "9.25"`, 1)
	if tampered == string(data) {
		t.Fatal("saved daily consolidation did not contain the expected amount")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadDailyConsolidation(context.Background(), record.UTCDate)
	if err == nil || loaded != nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("LoadDailyConsolidation() = %#v, %v; want checksum error", loaded, err)
	}
}

func TestStoreSaveDailyConsolidationReturnsTempCleanupError(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	cleanupErr := errors.New("remove daily consolidation temp denied")
	var removedPath string
	store.removeTempFile = func(path string) error {
		removedPath = path
		return cleanupErr
	}
	record := testDailyConsolidation()

	err := store.SaveDailyConsolidation(context.Background(), record)
	if !errors.Is(err, cleanupErr) ||
		!strings.Contains(err.Error(), "remove daily consolidation temp file") {
		t.Fatalf("SaveDailyConsolidation() error = %v, want temp cleanup error", err)
	}
	if base := filepath.Base(removedPath); !strings.HasPrefix(base, ".daily-consolidation.tmp-") {
		t.Fatalf("removed temp path = %q", removedPath)
	}
	loaded, loadErr := NewStore(dir).LoadDailyConsolidation(context.Background(), record.UTCDate)
	if loadErr != nil || loaded == nil || loaded.Status != record.Status {
		t.Fatalf("saved daily consolidation = %#v, %v", loaded, loadErr)
	}
}

func TestDailyConsolidationStoreHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := NewStore(t.TempDir())
	if err := store.SaveDailyConsolidation(ctx, testDailyConsolidation()); err != context.Canceled {
		t.Fatalf("SaveDailyConsolidation() error = %v, want context.Canceled", err)
	}
	if record, err := store.LoadDailyConsolidation(ctx, "2026-07-12"); err != context.Canceled ||
		record != nil {
		t.Fatalf("LoadDailyConsolidation() = %#v, %v; want context.Canceled", record, err)
	}
}

func testDailyConsolidation() funding.DailyConsolidation {
	return funding.DailyConsolidation{
		UTCDate: "2026-07-12", Status: funding.DailyConsolidationCompleted,
		StartedAt:            time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC),
		ClaimablesCapturedAt: time.Date(2026, 7, 12, 0, 0, 1, 0, time.UTC),
		CompletedAt:          time.Date(2026, 7, 12, 0, 0, 2, 0, time.UTC),
		Builders: []funding.DailyConsolidationBuilder{
			{
				Address: "0xbuilder-1", ClaimableUSDC: "1.25", ClaimableObserved: true,
				ClaimableQueryAttempts: 1, ClaimAttempts: 1,
				ClaimStatus: funding.ReportStepSuccess, SweepAmount: "1.25",
				SweepStatus: funding.ReportStepSuccess,
			},
			{
				Address: "0xbuilder-2", ClaimableUSDC: "2", ClaimableObserved: true,
				ClaimableQueryAttempts: 2, ClaimAttempts: 3, ClaimUncertain: true,
				ClaimStatus: funding.ReportStepWarning,
				SweepStatus: funding.ReportStepWarning,
			},
		},
	}
}
