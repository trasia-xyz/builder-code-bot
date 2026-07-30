package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"builder-code-bot/internal/funding"
)

const (
	claimableSnapshotDirname       = "claimable-snapshots"
	claimableSnapshotSchemaVersion = 1
)

type claimableSnapshotEnvelope struct {
	SchemaVersion int                       `json:"schema_version"`
	Checksum      string                    `json:"checksum"`
	Snapshot      funding.ClaimableSnapshot `json:"snapshot"`
}

var _ funding.ClaimableSnapshotStore = (*Store)(nil)

func (s *Store) LoadClaimableSnapshot(
	ctx context.Context,
	utcDate string,
) (*funding.ClaimableSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateUTCDate(utcDate); err != nil {
		return nil, err
	}
	path := filepath.Join(s.dataDir, claimableSnapshotDirname, utcDate+".json")
	snapshot, exists, err := loadClaimableSnapshot(path)
	if err != nil {
		return nil, fmt.Errorf("load claimable snapshot %s: %w", utcDate, err)
	}
	if !exists {
		return nil, nil
	}
	if snapshot.UTCDate != utcDate {
		return nil, fmt.Errorf(
			"claimable snapshot date %q does not match requested date %q",
			snapshot.UTCDate, utcDate,
		)
	}
	return snapshot, nil
}

func (s *Store) SaveClaimableSnapshot(
	ctx context.Context,
	snapshot funding.ClaimableSnapshot,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	if err := ensurePrivateDir(s.dataDir); err != nil {
		return fmt.Errorf("prepare state directory: %w", err)
	}
	dir := filepath.Join(s.dataDir, claimableSnapshotDirname)
	if err := ensurePrivateDir(dir); err != nil {
		return fmt.Errorf("prepare claimable snapshot directory: %w", err)
	}
	path := filepath.Join(dir, snapshot.UTCDate+".json")
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("claimable snapshot for %s already exists", snapshot.UTCDate)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check existing claimable snapshot: %w", err)
	}

	tempPath, err := writeSyncedClaimableSnapshotTemp(dir, snapshot)
	if err != nil {
		return fmt.Errorf("write claimable snapshot temp file: %w", err)
	}
	defer os.Remove(tempPath)
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("install claimable snapshot: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("sync claimable snapshot directory: %w", err)
	}
	if err := syncDir(s.dataDir); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}

func loadClaimableSnapshot(path string) (*funding.ClaimableSnapshot, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, true, fmt.Errorf("snapshot is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, true, fmt.Errorf(
			"snapshot permissions %04o allow group or other access",
			info.Mode().Perm(),
		)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, true, err
	}
	snapshot, err := unmarshalClaimableSnapshotEnvelope(data)
	if err != nil {
		return nil, true, err
	}
	return snapshot, true, nil
}

func writeSyncedClaimableSnapshotTemp(
	dir string,
	snapshot funding.ClaimableSnapshot,
) (path string, err error) {
	file, err := os.CreateTemp(dir, ".claimable-snapshot.tmp-*")
	if err != nil {
		return "", err
	}
	path = file.Name()
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
		if err != nil {
			os.Remove(path)
		}
	}()
	if err = file.Chmod(0o600); err != nil {
		return "", err
	}
	data, err := marshalClaimableSnapshotEnvelope(snapshot)
	if err != nil {
		return "", err
	}
	if _, err = file.Write(data); err != nil {
		return "", err
	}
	if err = file.Sync(); err != nil {
		return "", err
	}
	return path, nil
}

func marshalClaimableSnapshotEnvelope(snapshot funding.ClaimableSnapshot) ([]byte, error) {
	snapshotJSON, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode claimable snapshot: %w", err)
	}
	digest := sha256.Sum256(snapshotJSON)
	data, err := json.Marshal(claimableSnapshotEnvelope{
		SchemaVersion: claimableSnapshotSchemaVersion,
		Checksum:      hex.EncodeToString(digest[:]),
		Snapshot:      snapshot,
	})
	if err != nil {
		return nil, fmt.Errorf("encode claimable snapshot envelope: %w", err)
	}
	return data, nil
}

func unmarshalClaimableSnapshotEnvelope(data []byte) (*funding.ClaimableSnapshot, error) {
	var saved claimableSnapshotEnvelope
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, fmt.Errorf("decode claimable snapshot envelope: %w", err)
	}
	if saved.SchemaVersion != claimableSnapshotSchemaVersion {
		return nil, fmt.Errorf(
			"unsupported claimable snapshot schema version %d",
			saved.SchemaVersion,
		)
	}
	snapshotJSON, err := json.Marshal(saved.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode claimable snapshot for checksum: %w", err)
	}
	digest := sha256.Sum256(snapshotJSON)
	if want := hex.EncodeToString(digest[:]); saved.Checksum != want {
		return nil, fmt.Errorf("claimable snapshot checksum mismatch")
	}
	if err := saved.Snapshot.Validate(); err != nil {
		return nil, err
	}
	return &saved.Snapshot, nil
}

func validateUTCDate(value string) error {
	parsed, err := time.Parse(time.DateOnly, value)
	if err != nil || parsed.Format(time.DateOnly) != value {
		return fmt.Errorf("invalid UTC date %q", value)
	}
	return nil
}
