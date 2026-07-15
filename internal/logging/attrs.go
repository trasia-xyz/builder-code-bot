package logging

import (
	"log/slog"
	"time"
)

const (
	consolePrefixKey    = "_console_prefix"
	consoleSeparatorKey = "_console_separator"
)

func Component(value string) slog.Attr {
	return slog.String("component", value)
}

func ActionID(value string) slog.Attr {
	return slog.String("action_id", value)
}

func Coin(value string) slog.Attr {
	return slog.String("coin", value)
}

func OwnerID(value string) slog.Attr {
	return slog.String("owner_id", value)
}

func FencingToken(value int64) slog.Attr {
	return slog.Int64("fencing_token", value)
}

func Dex(value string) slog.Attr {
	return slog.String("dex", value)
}

func ConsolePrefix(value string) slog.Attr {
	return slog.String(consolePrefixKey, value)
}

// ConsoleSeparator inserts a blank line before the log record in console
// format. JSON output drops the presentation-only attribute so it remains
// valid newline-delimited JSON.
func ConsoleSeparator() slog.Attr {
	return slog.Bool(consoleSeparatorKey, true)
}

func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}
	return slog.String("error", err.Error())
}

func String(key, value string) slog.Attr {
	return slog.String(key, value)
}

func Bool(key string, value bool) slog.Attr {
	return slog.Bool(key, value)
}

func Int(key string, value int) slog.Attr {
	return slog.Int(key, value)
}

func Int64(key string, value int64) slog.Attr {
	return slog.Int64(key, value)
}

func Uint64(key string, value uint64) slog.Attr {
	return slog.Uint64(key, value)
}

func Duration(key string, value time.Duration) slog.Attr {
	return slog.Duration(key, value)
}

func Time(key string, value time.Time) slog.Attr {
	return slog.Time(key, value.UTC())
}

func Any(key string, value any) slog.Attr {
	return slog.Any(key, value)
}
