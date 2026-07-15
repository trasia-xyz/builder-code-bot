package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"time"
)

const (
	loggerCallerSkip       = 3
	logHandleErrorInterval = 30 * time.Second
)

var logHandleErrorReporter = handleErrorReporter{interval: logHandleErrorInterval}

type Config struct {
	Format        string
	Level         string
	Output        io.Writer
	AddSource     bool
	Component     string
	Color         string
	SensitiveKeys []string
}

type Logger struct {
	logger    *slog.Logger
	addSource bool
	matcher   sensitiveMatcher
}

// New creates a logger from cfg without installing it as the process-wide
// default logger.
func New(cfg Config) Logger {
	output := cfg.Output
	if output == nil {
		output = os.Stdout
	}
	parsedLevel, err := ParseLevel(cfg.Level)
	if err != nil {
		parsedLevel = slog.LevelInfo
	}
	// slog.Level is itself a slog.Leveler, so the handlers below keep an
	// internal Leveler while the public Config only accepts a string.
	var level slog.Leveler = parsedLevel
	sensitiveKeys := append([]string(nil), defaultSensitiveKeys...)
	sensitiveKeys = append(sensitiveKeys, cfg.SensitiveKeys...)
	matcher := newSensitiveMatcher(sensitiveKeys)

	format := normalizeFormat(cfg.Format)
	color := normalizeColor(cfg.Color)

	var handler slog.Handler
	if format == FormatConsole {
		handler = newConsoleHandler(output, level, cfg.AddSource, shouldColor(format, color, output), sensitiveKeys)
	} else {
		handler = slog.NewJSONHandler(output, &slog.HandlerOptions{
			AddSource:   cfg.AddSource,
			Level:       level,
			ReplaceAttr: redactReplaceAttr(sensitiveKeys),
		})
	}
	logger := slog.New(handler)
	if cfg.Component != "" {
		logger = logger.With(Component(cfg.Component))
	}
	return Logger{logger: logger, addSource: cfg.AddSource, matcher: matcher}
}

func redactReplaceAttr(keys []string) func(groups []string, attr slog.Attr) slog.Attr {
	matcher := newSensitiveMatcher(keys)
	return func(groups []string, attr slog.Attr) slog.Attr {
		if attr.Key == slog.TimeKey {
			return slog.Time(slog.TimeKey, attr.Value.Time().UTC())
		}
		if attr.Key == slog.LevelKey {
			if level, ok := attr.Value.Any().(slog.Level); ok {
				return slog.String(slog.LevelKey, levelName(level))
			}
			return slog.String(slog.LevelKey, attr.Value.String())
		}
		if attr.Key == slog.SourceKey {
			return trimSourceAttr(attr)
		}
		if attr.Key == consolePrefixKey || attr.Key == consoleSeparatorKey {
			return slog.Attr{}
		}
		return matcher.redactKey(attr, joinGroupKey(groups, attr.Key))
	}
}

func (l Logger) With(attrs ...slog.Attr) (out Logger) {
	base := l
	if base.logger == nil {
		base = Default()
	}
	if base.logger == nil {
		return base
	}
	out = base
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "logging: recovered panic while deriving logger: %v\n", r)
			out = base
		}
	}()
	attrs = base.matcher.sanitizeAttrs(attrs)
	return Logger{
		logger:    slog.New(base.logger.Handler().WithAttrs(attrs)),
		addSource: base.addSource,
		matcher:   base.matcher,
	}
}

func (l Logger) WithComponent(component string) Logger {
	return l.With(Component(component))
}

func (l Logger) Debug(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.log(ctx, slog.LevelDebug, msg, attrs...)
}

func (l Logger) Info(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.log(ctx, slog.LevelInfo, msg, attrs...)
}

func (l Logger) Warn(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.log(ctx, slog.LevelWarn, msg, attrs...)
}

func (l Logger) Error(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.log(ctx, slog.LevelError, msg, attrs...)
}

func (l Logger) log(ctx context.Context, level slog.Level, msg string, attrs ...slog.Attr) {
	// A logging call must never take the process down. Resolving user-supplied
	// values can run third-party String()/MarshalJSON() code that may panic;
	// contain it here and report to stderr instead of crashing.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "logging: recovered panic while emitting log: %v\n", r)
		}
	}()

	base := l
	if base.logger == nil {
		base = Default()
		if base.logger == nil {
			return
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !base.logger.Enabled(ctx, level) {
		return
	}
	var pc uintptr
	if base.addSource {
		var pcs [1]uintptr
		runtime.Callers(loggerCallerSkip, pcs[:])
		pc = pcs[0]
	}
	record := slog.NewRecord(time.Now(), level, msg, pc)
	record.AddAttrs(base.matcher.sanitizeAttrs(attrs)...)
	if err := base.logger.Handler().Handle(ctx, record); err != nil {
		logHandleErrorReporter.report(time.Now(), level, msg, err)
	}
}

type handleErrorReporter struct {
	mu         sync.Mutex
	interval   time.Duration
	lastReport time.Time
	suppressed int
}

func (r *handleErrorReporter) report(now time.Time, level slog.Level, msg string, err error) {
	if err == nil {
		return
	}
	shouldReport, suppressed := r.shouldReport(now)
	if !shouldReport {
		return
	}
	if suppressed > 0 {
		fmt.Fprintf(os.Stderr, "logging: failed to write log record level=%s msg=%q error=%v suppressed=%d\n",
			levelName(level), msg, err, suppressed)
		return
	}
	fmt.Fprintf(os.Stderr, "logging: failed to write log record level=%s msg=%q error=%v\n",
		levelName(level), msg, err)
}

func (r *handleErrorReporter) shouldReport(now time.Time) (bool, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.interval <= 0 || r.lastReport.IsZero() || now.Sub(r.lastReport) >= r.interval {
		suppressed := r.suppressed
		r.suppressed = 0
		r.lastReport = now
		return true, suppressed
	}
	r.suppressed++
	return false, 0
}
