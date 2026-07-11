package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type ProcessLock struct {
	file *os.File
}

func AcquireProcessLock(dataDir string) (*ProcessLock, error) {
	if err := ensurePrivateDir(dataDir); err != nil {
		return nil, fmt.Errorf("prepare process lock directory: %w", err)
	}
	path := filepath.Join(dataDir, "LOCK")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open process lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, fmt.Errorf("set process lock permissions: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("process lock is already locked")
		}
		return nil, fmt.Errorf("acquire process lock: %w", err)
	}
	return &ProcessLock{file: file}, nil
}

func (l *ProcessLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}
