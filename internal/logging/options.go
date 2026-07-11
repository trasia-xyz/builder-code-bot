package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

const (
	FormatConsole = "console"
	FormatJSON    = "json"

	ColorAuto  = "auto"
	ColorNever = "never"

	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

func ParseLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", LevelInfo:
		return slog.LevelInfo, nil
	case LevelDebug:
		return slog.LevelDebug, nil
	case LevelWarn:
		return slog.LevelWarn, nil
	case LevelError:
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unknown log level %q", value)
	}
}

func MustParseLevel(value string) slog.Level {
	level, err := ParseLevel(value)
	if err != nil {
		panic(err)
	}
	return level
}

func ValidateFormat(value string) error {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case FormatConsole, FormatJSON:
		return nil
	default:
		return fmt.Errorf("must be %q or %q",
			FormatConsole, FormatJSON,
		)
	}
}

func ValidateLevel(value string) error {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case LevelDebug, LevelInfo, LevelWarn, LevelError:
		return nil
	default:
		return fmt.Errorf("must be one of %q, %q, %q, %q",
			LevelDebug, LevelInfo, LevelWarn, LevelError,
		)
	}
}

func ValidateColor(value string) error {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ColorAuto, ColorNever:
		return nil
	default:
		return fmt.Errorf("must be %q or %q",
			ColorAuto, ColorNever,
		)
	}
}

func levelName(level slog.Level) string {
	switch level {
	case slog.LevelDebug:
		return "DEBUG"
	case slog.LevelInfo:
		return "INFO"
	case slog.LevelWarn:
		return "WARN"
	case slog.LevelError:
		return "ERROR"
	default:
		return level.String()
	}
}

func normalizeFormat(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case FormatConsole:
		return FormatConsole
	case "", FormatJSON:
		return FormatJSON
	default:
		return FormatConsole
	}
}

func normalizeColor(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", ColorAuto:
		return ColorAuto
	case ColorNever:
		return ColorNever
	default:
		return ColorAuto
	}
}

func shouldColor(format, color string, output io.Writer) bool {
	if format != FormatConsole {
		return false
	}
	if normalizeColor(color) == ColorNever {
		return false
	}
	file, ok := output.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
