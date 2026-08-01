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
	dailyConsolidationDirname       = "daily-consolidations"
	dailyConsolidationSchemaVersion = 1
)

type dailyConsolidationEnvelope struct {
	SchemaVersion int                        `json:"schema_version"`
	Checksum      string                     `json:"checksum"`
	Consolidation funding.DailyConsolidation `json:"consolidation"`
}

var _ funding.DailyConsolidationStore = (*Store)(nil)

func (s *Store) LoadDailyConsolidation(
	ctx context.Context,
	utcDate string,
) (*funding.DailyConsolidation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateUTCDate(utcDate); err != nil {
		return nil, err
	}
	path := filepath.Join(s.dataDir, dailyConsolidationDirname, utcDate+".json")
	consolidation, exists, err := loadDailyConsolidation(path)
	if err != nil {
		return nil, fmt.Errorf("load daily consolidation %s: %w", utcDate, err)
	}
	if !exists {
		return nil, nil
	}
	if consolidation.UTCDate != utcDate {
		return nil, fmt.Errorf(
			"daily consolidation date %q does not match requested date %q",
			consolidation.UTCDate, utcDate,
		)
	}
	return consolidation, nil
}

func (s *Store) SaveDailyConsolidation(
	ctx context.Context,
	consolidation funding.DailyConsolidation,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := consolidation.Validate(); err != nil {
		return err
	}
	if err := ensurePrivateDir(s.dataDir); err != nil {
		return fmt.Errorf("prepare state directory: %w", err)
	}
	dir := filepath.Join(s.dataDir, dailyConsolidationDirname)
	if err := ensurePrivateDir(dir); err != nil {
		return fmt.Errorf("prepare daily consolidation directory: %w", err)
	}
	path := filepath.Join(dir, consolidation.UTCDate+".json")
	tempPath, err := writeSyncedDailyConsolidationTemp(dir, consolidation)
	if err != nil {
		return fmt.Errorf("write daily consolidation temp file: %w", err)
	}
	defer os.Remove(tempPath)
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("install daily consolidation: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("sync daily consolidation directory: %w", err)
	}
	if err := syncDir(s.dataDir); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}

func loadDailyConsolidation(path string) (*funding.DailyConsolidation, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, true, fmt.Errorf("daily consolidation is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, true, fmt.Errorf(
			"daily consolidation permissions %04o allow group or other access",
			info.Mode().Perm(),
		)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, true, err
	}
	consolidation, err := unmarshalDailyConsolidationEnvelope(data)
	if err != nil {
		return nil, true, err
	}
	return consolidation, true, nil
}

func writeSyncedDailyConsolidationTemp(
	dir string,
	consolidation funding.DailyConsolidation,
) (path string, err error) {
	file, err := os.CreateTemp(dir, ".daily-consolidation.tmp-*")
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
	data, err := marshalDailyConsolidationEnvelope(consolidation)
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

func marshalDailyConsolidationEnvelope(consolidation funding.DailyConsolidation) ([]byte, error) {
	consolidationJSON, err := json.Marshal(consolidation)
	if err != nil {
		return nil, fmt.Errorf("encode daily consolidation: %w", err)
	}
	digest := sha256.Sum256(consolidationJSON)
	data, err := json.MarshalIndent(dailyConsolidationEnvelope{
		SchemaVersion: dailyConsolidationSchemaVersion,
		Checksum:      hex.EncodeToString(digest[:]),
		Consolidation: consolidation,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode daily consolidation envelope: %w", err)
	}
	return append(data, '\n'), nil
}

func unmarshalDailyConsolidationEnvelope(data []byte) (*funding.DailyConsolidation, error) {
	var saved dailyConsolidationEnvelope
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, fmt.Errorf("decode daily consolidation envelope: %w", err)
	}
	if saved.SchemaVersion != dailyConsolidationSchemaVersion {
		return nil, fmt.Errorf(
			"unsupported daily consolidation schema version %d",
			saved.SchemaVersion,
		)
	}
	consolidationJSON, err := json.Marshal(saved.Consolidation)
	if err != nil {
		return nil, fmt.Errorf("encode daily consolidation for checksum: %w", err)
	}
	digest := sha256.Sum256(consolidationJSON)
	if want := hex.EncodeToString(digest[:]); saved.Checksum != want {
		return nil, fmt.Errorf("daily consolidation checksum mismatch")
	}
	if err := saved.Consolidation.Validate(); err != nil {
		return nil, err
	}
	return &saved.Consolidation, nil
}

func validateUTCDate(value string) error {
	parsed, err := time.Parse(time.DateOnly, value)
	if err != nil || parsed.Format(time.DateOnly) != value {
		return fmt.Errorf("invalid UTC date %q", value)
	}
	return nil
}
