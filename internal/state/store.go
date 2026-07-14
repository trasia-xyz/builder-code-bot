package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"hyperliquid-builder-code-bot/internal/funding"
)

const DataDir = "./data"

const (
	currentFilename = "current.json"
	backupFilename  = "current.json.bak"
	historyDirname  = "history"
)

type Store struct {
	dataDir string
}

var _ funding.StateStore = (*Store)(nil)

func NewStore(dataDir string) *Store {
	return &Store{dataDir: dataDir}
}

func (s *Store) LoadWithMetadata(ctx context.Context) (*funding.RunState, funding.StateLoadMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, funding.StateLoadMetadata{}, err
	}
	currentPath := filepath.Join(s.dataDir, currentFilename)
	current, currentExists, currentErr := loadSnapshot(currentPath)
	if currentErr == nil && currentExists {
		return current, funding.StateLoadMetadata{}, nil
	}

	backupPath := filepath.Join(s.dataDir, backupFilename)
	backup, backupExists, backupErr := loadSnapshot(backupPath)
	if backupErr == nil && backupExists {
		return backup, funding.StateLoadMetadata{
			RecoveredFromBackup: true,
			PrimaryInvalid:      currentErr != nil,
		}, nil
	}
	if !currentExists && currentErr == nil && !backupExists && backupErr == nil {
		return nil, funding.StateLoadMetadata{}, nil
	}
	if currentErr != nil && backupErr != nil {
		return nil, funding.StateLoadMetadata{}, fmt.Errorf("load snapshots: %s: %v; %s: %v", currentFilename, currentErr, backupFilename, backupErr)
	}
	if currentErr != nil {
		return nil, funding.StateLoadMetadata{}, fmt.Errorf("load %s: %w", currentFilename, currentErr)
	}
	return nil, funding.StateLoadMetadata{}, fmt.Errorf("load %s: %w", backupFilename, backupErr)
}

func (s *Store) Save(ctx context.Context, state funding.RunState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensurePrivateDir(s.dataDir); err != nil {
		return fmt.Errorf("prepare state directory: %w", err)
	}
	tempPath, err := writeSyncedEnvelopeTemp(s.dataDir, ".current.json.tmp-*", state)
	if err != nil {
		return fmt.Errorf("write current snapshot temp file: %w", err)
	}
	defer os.Remove(tempPath)

	currentPath := filepath.Join(s.dataDir, currentFilename)
	backupPath := filepath.Join(s.dataDir, backupFilename)
	if _, exists, loadErr := loadSnapshot(currentPath); exists && loadErr == nil {
		if err := os.Rename(currentPath, backupPath); err != nil {
			return fmt.Errorf("rotate current snapshot to backup: %w", err)
		}
	} else if loadErr != nil {
		if _, backupExists, backupErr := loadSnapshot(backupPath); !backupExists || backupErr != nil {
			return fmt.Errorf("refuse to replace invalid current snapshot without a valid backup: %w", loadErr)
		}
	}
	if err := os.Rename(tempPath, currentPath); err != nil {
		return fmt.Errorf("install current snapshot: %w", err)
	}
	if err := syncDir(s.dataDir); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}

func (s *Store) Archive(ctx context.Context, state funding.RunState, result string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateFilenamePart("UTC date", state.UTCDate); err != nil {
		return err
	}
	if err := validateFilenamePart("run ID", state.RunID); err != nil {
		return err
	}
	if err := validateFilenamePart("result", result); err != nil {
		return err
	}
	if err := ensurePrivateDir(s.dataDir); err != nil {
		return fmt.Errorf("prepare state directory: %w", err)
	}
	historyDir := filepath.Join(s.dataDir, historyDirname)
	if err := ensurePrivateDir(historyDir); err != nil {
		return fmt.Errorf("prepare history directory: %w", err)
	}
	tempPath, err := writeSyncedEnvelopeTemp(historyDir, ".history.tmp-*", state)
	if err != nil {
		return fmt.Errorf("write history temp file: %w", err)
	}
	defer os.Remove(tempPath)
	filename := fmt.Sprintf("%s-%s-%s.json", state.UTCDate, state.RunID, result)
	if err := os.Rename(tempPath, filepath.Join(historyDir, filename)); err != nil {
		return fmt.Errorf("install history snapshot: %w", err)
	}
	if err := syncDir(historyDir); err != nil {
		return fmt.Errorf("sync history directory: %w", err)
	}
	if err := syncDir(s.dataDir); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}

func (s *Store) Clear(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensurePrivateDir(s.dataDir); err != nil {
		return fmt.Errorf("prepare state directory: %w", err)
	}
	for _, name := range []string{currentFilename, backupFilename} {
		if err := os.Remove(filepath.Join(s.dataDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", name, err)
		}
	}
	if err := syncDir(s.dataDir); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}

func loadSnapshot(path string) (*funding.RunState, bool, error) {
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
		return nil, true, fmt.Errorf("snapshot permissions %04o allow group or other access", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, true, err
	}
	state, err := unmarshalEnvelope(data)
	if err != nil {
		return nil, true, err
	}
	return state, true, nil
}

func ensurePrivateDir(path string) error {
	if path == "" {
		return fmt.Errorf("directory path is empty")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func writeSyncedEnvelopeTemp(dir, pattern string, state funding.RunState) (path string, err error) {
	file, err := os.CreateTemp(dir, pattern)
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
	data, err := marshalEnvelope(state)
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

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return err
	}
	return nil
}

func validateFilenamePart(name, value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\\`) {
		return fmt.Errorf("invalid %s %q for archive filename", name, value)
	}
	return nil
}
