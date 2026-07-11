package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"hyperliquid-builder-code-bot/internal/funding"
)

func TestStoreLoadReturnsNilWhenNoSnapshotExists(t *testing.T) {
	got, err := NewStore(t.TempDir()).Load(context.Background())
	if err != nil || got != nil {
		t.Fatalf("Load() = %#v, %v; want nil, nil", got, err)
	}
}

func TestStoreFallsBackToValidBackup(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	first := testRunState(t, "first")
	second := testRunState(t, "second")
	if err := store.Save(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "current.json"), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := store.Load(context.Background())
	if err != nil || got == nil || got.RunID != "first" {
		t.Fatalf("Load() = %#v, %v; want backup run first", got, err)
	}
}

func TestStoreLoadWithMetadataReportsInvalidPrimaryBackupRecovery(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	first := testRunState(t, "first")
	second := testRunState(t, "second")
	if err := store.Save(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "current.json"), []byte("private_key=primary-secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, metadata, err := store.LoadWithMetadata(context.Background())
	if err != nil || got == nil || got.RunID != "first" {
		t.Fatalf("LoadWithMetadata() = %#v, %#v, %v", got, metadata, err)
	}
	if !metadata.RecoveredFromBackup || !metadata.PrimaryInvalid {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestStoreRejectsChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save(context.Background(), testRunState(t, "original")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "current.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(data), `"run_id":"original"`, `"run_id":"tampered"`, 1)
	if tampered == string(data) {
		t.Fatal("snapshot did not contain run ID")
	}
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}

	if got, err := store.Load(context.Background()); err == nil || got != nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("Load() = %#v, %v; want checksum error", got, err)
	}
}

func TestStoreRejectsManifestHashMismatch(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	state := testRunState(t, "tampered-manifest")
	state.Manifest.ManifestHash = strings.Repeat("0", 64)
	writeTestEnvelope(t, filepath.Join(dir, "current.json"), state)

	if got, err := store.Load(context.Background()); err == nil || got != nil || !strings.Contains(err.Error(), "manifest hash") {
		t.Fatalf("Load() = %#v, %v; want manifest hash error", got, err)
	}
}

func TestStoreRejectsMissingManifestHash(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	state := testRunState(t, "missing-manifest-hash")
	state.Manifest.ManifestHash = ""
	writeTestEnvelope(t, filepath.Join(dir, "current.json"), state)

	if got, err := store.Load(context.Background()); err == nil || got != nil || !strings.Contains(err.Error(), "manifest hash") {
		t.Fatalf("Load() = %#v, %v; want manifest hash error", got, err)
	}
}

func TestStoreRejectsUnsupportedSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	state := testRunState(t, "future")
	path := filepath.Join(dir, "current.json")
	writeTestEnvelopeVersion(t, path, state, 2)

	if got, err := NewStore(dir).Load(context.Background()); err == nil || got != nil || !strings.Contains(err.Error(), "unsupported state schema version 2") {
		t.Fatalf("Load() = %#v, %v", got, err)
	}
}

func TestStoreReportsErrorWhenPrimaryAndBackupAreInvalid(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"current.json", "current.json.bak"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("broken"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := NewStore(dir).Load(context.Background())
	if err == nil || got != nil || !strings.Contains(err.Error(), "current.json") || !strings.Contains(err.Error(), "current.json.bak") {
		t.Fatalf("Load() = %#v, %v; want both snapshot errors", got, err)
	}
}

func TestStoreEnforcesPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not available")
	}
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	store := NewStore(dir)
	if err := store.Save(context.Background(), testRunState(t, "first")); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), testRunState(t, "second")); err != nil {
		t.Fatal(err)
	}

	assertMode(t, dir, 0o700)
	assertMode(t, filepath.Join(dir, "current.json"), 0o600)
	assertMode(t, filepath.Join(dir, "current.json.bak"), 0o600)
}

func TestStoreArchivesBeforeClear(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	state := testRunState(t, "run-1")
	if err := store.Save(context.Background(), testRunState(t, "previous-run")); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if err := store.Archive(context.Background(), state, "completed"); err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"current.json", "current.json.bak"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s still exists or stat failed: %v", name, err)
		}
	}
	archive := filepath.Join(dir, "history", "2026-07-11-run-1-completed.json")
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	var saved testEnvelope
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.State.RunID != state.RunID {
		t.Fatalf("archived run ID = %q, want %q", saved.State.RunID, state.RunID)
	}
	loaded, exists, err := loadSnapshot(archive)
	if err != nil || !exists || loaded == nil || loaded.RunID != state.RunID {
		t.Fatalf("loadSnapshot(archive) = %#v, %v, %v", loaded, exists, err)
	}
	assertMode(t, filepath.Join(dir, "history"), 0o700)
	assertMode(t, archive, 0o600)
}

func TestStoreTerminalArchivesRemainEnvelopeReadable(t *testing.T) {
	tests := []struct {
		name     string
		result   string
		manifest funding.Manifest
	}{
		{name: "no data", result: "no_data", manifest: func() funding.Manifest {
			manifest, err := funding.BuildManifest(funding.ManifestInput{
				Builders: []string{"0xBuilder"}, Settlement: "0xSettlement", Recipient: "0xRecipient",
			})
			if err != nil {
				t.Fatal(err)
			}
			return manifest
		}()},
		{name: "failed validation", result: "failed_validation", manifest: func() funding.Manifest {
			manifest := funding.Manifest{
				Records:  []funding.Record{{ID: 1, Amount: "-0.01"}},
				RawTotal: "unavailable", PayoutTotal: "unavailable",
				Builders: []string{"0xBuilder"}, Settlement: "0xSettlement", Recipient: "0xRecipient",
			}
			hash, err := funding.HashManifest(manifest)
			if err != nil {
				t.Fatal(err)
			}
			manifest.ManifestHash = hash
			return manifest
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			store := NewStore(dir)
			state := testRunState(t, "terminal")
			state.Manifest = tt.manifest
			if err := store.Archive(context.Background(), state, tt.result); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "history", "2026-07-11-terminal-"+tt.result+".json")
			loaded, exists, err := loadSnapshot(path)
			if err != nil || !exists || loaded == nil || loaded.Manifest.ManifestHash != tt.manifest.ManifestHash {
				t.Fatalf("loadSnapshot() = %#v, %v, %v", loaded, exists, err)
			}
		})
	}
}

func TestStoreMethodsHonorCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := NewStore(t.TempDir())
	state := testRunState(t, "canceled")
	for name, call := range map[string]func() error{
		"save":    func() error { return store.Save(ctx, state) },
		"archive": func() error { return store.Archive(ctx, state, "failed") },
		"clear":   func() error { return store.Clear(ctx) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err != context.Canceled {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
		})
	}
	if got, err := store.Load(ctx); err != context.Canceled || got != nil {
		t.Fatalf("Load() = %#v, %v; want nil, context.Canceled", got, err)
	}
}

type testEnvelope struct {
	SchemaVersion int              `json:"schema_version"`
	Checksum      string           `json:"checksum"`
	State         funding.RunState `json:"state"`
}

func testRunState(t *testing.T, runID string) funding.RunState {
	t.Helper()
	manifest, err := funding.BuildManifest(funding.ManifestInput{
		Records:    []funding.Record{{ID: 1, PeriodStartAt: 1, Amount: "0.000000000000000000"}},
		Builders:   []string{"0xBuilder"},
		Settlement: "0xSettlement",
		Recipient:  "0xRecipient",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	return funding.RunState{
		RunID: runID, Trigger: funding.TriggerUTC, UTCDate: "2026-07-11",
		Phase: funding.PhasePrepared, Manifest: manifest, CreatedAt: now, UpdatedAt: now,
	}
}

func writeTestEnvelope(t *testing.T, path string, state funding.RunState) {
	writeTestEnvelopeVersion(t, path, state, 1)
}

func writeTestEnvelopeVersion(t *testing.T, path string, state funding.RunState, version int) {
	t.Helper()
	stateJSON, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(stateJSON)
	data, err := json.Marshal(testEnvelope{
		SchemaVersion: version,
		Checksum:      hex.EncodeToString(digest[:]),
		State:         state,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}
