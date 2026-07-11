package logging

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ansiReset  = "\x1b[0m"
	ansiCyan   = "\x1b[36m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiRed    = "\x1b[31m"

	consoleTimeFormat = "2006-01-02T15:04:05.000000Z"
)

type consoleHandler struct {
	output    io.Writer
	level     slog.Leveler
	addSource bool
	color     bool
	matcher   sensitiveMatcher
	attrs     []slog.Attr
	groups    []string
	mu        *sync.Mutex
}

func newConsoleHandler(
	output io.Writer,
	level slog.Leveler,
	addSource,
	color bool,
	sensitiveKeys []string,
) slog.Handler {
	return &consoleHandler{
		output:    output,
		level:     level,
		addSource: addSource,
		color:     color,
		matcher:   newSensitiveMatcher(sensitiveKeys),
		mu:        new(sync.Mutex),
	}
}

func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *consoleHandler) Handle(_ context.Context, record slog.Record) error {
	when := record.Time
	if when.IsZero() {
		when = time.Now()
	}

	var prefixValue string
	groupPrefix := strings.Join(h.groups, ".")
	fields := make([]string, 0, len(h.attrs)+record.NumAttrs()+1)
	for _, attr := range h.attrs {
		h.appendAttr(&fields, &prefixValue, groupPrefix, attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		h.appendAttr(&fields, &prefixValue, groupPrefix, attr)
		return true
	})
	if h.addSource && record.PC != 0 {
		fields = append(fields, "source="+formatFieldValue(source(record.PC)))
	}

	message := record.Message
	if prefixValue != "" {
		message = prefixValue + " " + message
	}
	line := formatConsoleTime(when) + " " + h.formatLevel(record.Level) + " " + message
	if len(fields) > 0 {
		line += " " + strings.Join(fields, " ")
	}
	line += "\n"

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.output, line)
	return err
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &next
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.groups = append(append([]string(nil), h.groups...), name)
	return &next
}

func (h *consoleHandler) appendAttr(fields *[]string, prefixValue *string, prefix string, attr slog.Attr) {
	if isEmptyAttr(attr) {
		return
	}
	if attr.Key == consolePrefixKey {
		// ConsolePrefix controls the prefix of the whole message line, so it
		// only takes effect at the top level. Nested occurrences (inside a
		// group) are dropped rather than leaked as a field, matching the JSON
		// handler which drops the key everywhere.
		if prefix == "" {
			*prefixValue = formatValue(attr.Value)
		}
		return
	}
	key := joinKey(prefix, attr.Key)
	if attr.Value.Kind() == slog.KindGroup {
		nextPrefix := key
		for _, child := range attr.Value.Group() {
			h.appendAttr(fields, prefixValue, nextPrefix, child)
		}
		return
	}
	if attr.Key != "" && h.matcher.matches(key) {
		*fields = append(*fields, key+"="+formatFieldValue(redactedValue))
		return
	}
	attr.Value = attr.Value.Resolve()
	if attr.Value.Kind() == slog.KindGroup {
		nextPrefix := joinKey(prefix, attr.Key)
		for _, child := range attr.Value.Group() {
			h.appendAttr(fields, prefixValue, nextPrefix, child)
		}
		return
	}
	if attr.Key == "" {
		return
	}
	*fields = append(*fields, key+"="+formatFieldValue(formatValue(attr.Value)))
}

func (h *consoleHandler) formatLevel(level slog.Level) string {
	text := fmt.Sprintf("%-5s", levelName(level))
	if !h.color {
		return text
	}
	switch {
	case level >= slog.LevelError:
		return ansiRed + text + ansiReset
	case level >= slog.LevelWarn:
		return ansiYellow + text + ansiReset
	case level <= slog.LevelDebug:
		return ansiCyan + text + ansiReset
	default:
		return ansiGreen + text + ansiReset
	}
}

func joinKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func formatValue(value slog.Value) string {
	value = value.Resolve()
	switch value.Kind() {
	case slog.KindString:
		return value.String()
	case slog.KindTime:
		return formatConsoleTime(value.Time())
	case slog.KindDuration:
		return value.Duration().String()
	case slog.KindBool:
		return strconv.FormatBool(value.Bool())
	case slog.KindInt64:
		return strconv.FormatInt(value.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(value.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(value.Float64(), 'f', -1, 64)
	case slog.KindAny:
		return formatAny(value.Any())
	case slog.KindLogValuer:
		return formatValue(value.Resolve())
	default:
		return value.String()
	}
}

func formatConsoleTime(value time.Time) string {
	return value.UTC().Format(consoleTimeFormat)
}

func formatAny(value any) string {
	if value == nil {
		return "<nil>"
	}
	if stringer, ok := value.(fmt.Stringer); ok {
		return stringer.String()
	}
	if bytes, err := json.Marshal(value); err == nil {
		return string(bytes)
	}
	return fmt.Sprint(value)
}

func formatFieldValue(value string) string {
	if value == "" {
		return `""`
	}
	if strings.ContainsAny(value, " \t\n\r\"=") {
		return strconv.Quote(value)
	}
	return value
}
