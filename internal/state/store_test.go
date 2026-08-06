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

	"builder-code-bot/internal/funding"
)

func TestStoreLoadReturnsNilWhenNoSnapshotExists(t *testing.T) {
	got, err := load(context.Background(), NewStore(t.TempDir()))
	if err != nil || got != nil {
		t.Fatalf("Load() = %#v, %v; want nil, nil", got, err)
	}
}

func TestEnvelopeSchemaVersionRemainsTwo(t *testing.T) {
	if schemaVersion != 2 {
		t.Fatalf("schemaVersion = %d, want 2", schemaVersion)
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

	got, err := load(context.Background(), store)
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

	if got, err := load(context.Background(), store); err == nil || got != nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("Load() = %#v, %v; want checksum error", got, err)
	}
}

func TestStoreRejectsLegacySchemaVersionOne(t *testing.T) {
	dir := t.TempDir()
	state := testRunState(t, "legacy")
	path := filepath.Join(dir, "current.json")
	writeTestEnvelopeVersion(t, path, state, 1)

	if got, err := load(context.Background(), NewStore(dir)); err == nil || got != nil ||
		!strings.Contains(err.Error(), "unsupported state schema version 1") {
		t.Fatalf("Load() = %#v, %v", got, err)
	}
}

func TestStoreRejectsFutureSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	state := testRunState(t, "future")
	path := filepath.Join(dir, "current.json")
	writeTestEnvelopeVersion(t, path, state, 3)

	if got, err := load(context.Background(), NewStore(dir)); err == nil || got != nil ||
		!strings.Contains(err.Error(), "unsupported state schema version 3") {
		t.Fatalf("Load() = %#v, %v", got, err)
	}
}

func TestEnvelopeOmitsTransientConsolidationManifest(t *testing.T) {
	state := testRunState(t, "transient")
	state.Manifest = funding.Manifest{
		Records:   []funding.Record{{ID: 99, Amount: "99"}},
		Recipient: "transient-recipient-must-not-persist",
	}

	data, err := marshalEnvelope(state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "transient-recipient-must-not-persist") {
		t.Fatalf("transient manifest was persisted: %s", data)
	}
	loaded, err := unmarshalEnvelope(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Manifest.Records) != 0 || loaded.Manifest.Recipient != "" {
		t.Fatalf("transient manifest was restored: %#v", loaded.Manifest)
	}
}

func TestStoreReportsErrorWhenPrimaryAndBackupAreInvalid(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"current.json", "current.json.bak"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("broken"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := load(context.Background(), NewStore(dir))
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
	if saved.SchemaVersion != schemaVersion || len(saved.State.Targets) != 2 ||
		saved.State.Targets[0].Kind != funding.PayoutKindPointsRebate ||
		saved.State.Targets[1].Kind != funding.PayoutKindReferral {
		t.Fatalf("archived envelope targets = %#v", saved)
	}
	loaded, exists, err := loadSnapshot(archive)
	if err != nil || !exists || loaded == nil || loaded.RunID != state.RunID {
		t.Fatalf("loadSnapshot(archive) = %#v, %v, %v", loaded, exists, err)
	}
	assertMode(t, filepath.Join(dir, "history"), 0o700)
	assertMode(t, archive, 0o600)
}

func TestStoreTerminalArchiveRemainsEnvelopeReadable(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	state := testRunState(t, "terminal")
	state.Targets[1].Phase = funding.PhaseBlocked
	state.Targets[1].BlockedReason = "referral record validation failed"
	state.Targets[1].Manifest = funding.Manifest{
		Records: []funding.Record{{ID: 1, Amount: "-0.01"}}, RawTotal: "unavailable",
		PayoutTotal: "unavailable", Settlement: "0xSettlement", Recipient: "0xReferralRecipient",
	}
	if err := store.Archive(context.Background(), state, "failed_validation"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "history", "2026-07-11-terminal-failed_validation.json")
	loaded, exists, err := loadSnapshot(path)
	if err != nil || !exists || loaded == nil || len(loaded.Targets) != 2 ||
		loaded.Targets[1].Kind != funding.PayoutKindReferral ||
		loaded.Targets[1].Phase != funding.PhaseBlocked ||
		len(loaded.Targets[1].Manifest.Records) != 1 ||
		loaded.Targets[1].Manifest.Records[0].ID != 1 {
		t.Fatalf("loadSnapshot() = %#v, %v, %v", loaded, exists, err)
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
	if got, err := load(ctx, store); err != context.Canceled || got != nil {
		t.Fatalf("Load() = %#v, %v; want nil, context.Canceled", got, err)
	}
}

func load(ctx context.Context, store *Store) (*funding.RunState, error) {
	state, _, err := store.LoadWithMetadata(ctx)
	return state, err
}

type testEnvelope struct {
	SchemaVersion int              `json:"schema_version"`
	Checksum      string           `json:"checksum"`
	State         funding.RunState `json:"state"`
}

func testRunState(t *testing.T, runID string) funding.RunState {
	t.Helper()
	pointsRebateManifest, err := funding.BuildManifest(funding.ManifestInput{
		Records:    []funding.Record{{ID: 1, PeriodStartAt: 1, Amount: "0.000000000000000000"}},
		Settlement: "0xSettlement",
		Recipient:  "0xPointsRebateRecipient",
	})
	if err != nil {
		t.Fatal(err)
	}
	referralManifest, err := funding.BuildManifest(funding.ManifestInput{
		Records:    []funding.Record{{ID: 2, PeriodStartAt: 2, Amount: "0.000000000000000000"}},
		Settlement: "0xSettlement",
		Recipient:  "0xReferralRecipient",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	return funding.RunState{
		RunID: runID, Trigger: funding.TriggerUTC, UTCDate: "2026-07-11",
		Targets: []funding.PayoutTargetState{
			{
				Kind: funding.PayoutKindPointsRebate, Phase: funding.PhaseCompleted,
				Manifest: pointsRebateManifest,
			},
			{
				Kind: funding.PayoutKindReferral, Phase: funding.PhasePrepared,
				Manifest: referralManifest,
			},
		},
		CreatedAt: now, UpdatedAt: now,
	}
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
