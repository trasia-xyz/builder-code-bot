package logging

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
)

var (
	defaultMu     sync.RWMutex
	defaultLogger = New(Config{})
)

// SetDefault installs logger as the process-wide default logger. Startup code
// should use SetDefault(New(cfg)) when it needs to create a logger and install
// it as the process default.
//
// A zero-value Logger (one not built via New) carries a nil underlying
// *slog.Logger and would make slog.SetDefault panic, so it is rejected: the
// previous default is kept and the misuse is reported to stderr rather than
// taking the process down.
func SetDefault(logger Logger) {
	if logger.logger == nil {
		fmt.Fprintln(os.Stderr, "logging: ignoring SetDefault with an uninitialized Logger (use logging.New)")
		return
	}
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultLogger = logger
	slog.SetDefault(logger.logger)
}

func Default() Logger {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultLogger
}

func Debug(ctx context.Context, msg string, attrs ...slog.Attr) {
	Default().log(ctx, slog.LevelDebug, msg, attrs...)
}

func Info(ctx context.Context, msg string, attrs ...slog.Attr) {
	Default().log(ctx, slog.LevelInfo, msg, attrs...)
}

func Warn(ctx context.Context, msg string, attrs ...slog.Attr) {
	Default().log(ctx, slog.LevelWarn, msg, attrs...)
}

func Error(ctx context.Context, msg string, attrs ...slog.Attr) {
	Default().log(ctx, slog.LevelError, msg, attrs...)
}

func Debugf(format string, args ...any) {
	Default().log(context.Background(), slog.LevelDebug, fmt.Sprintf(format, args...))
}

func Infof(format string, args ...any) {
	Default().log(context.Background(), slog.LevelInfo, fmt.Sprintf(format, args...))
}

func Warnf(format string, args ...any) {
	Default().log(context.Background(), slog.LevelWarn, fmt.Sprintf(format, args...))
}

func Errorf(format string, args ...any) {
	Default().log(context.Background(), slog.LevelError, fmt.Sprintf(format, args...))
}
