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
	"unicode"
)

const (
	ansiReset    = "\x1b[0m"
	ansiCyan     = "\x1b[36m"
	ansiGreen    = "\x1b[32m"
	ansiYellow   = "\x1b[33m"
	ansiRed      = "\x1b[31m"
	ansiBoldCyan = "\x1b[1;36m"

	consoleTimeFormat     = "2006-01-02T15:04:05.000000Z"
	consoleFieldSeparator = "  │  "
	consoleEventKey       = "event"

	consoleFieldPriorityNormal    = 0
	consoleFieldPriorityAmount    = 1
	consoleFieldPriorityProminent = 2
)

type consoleField struct {
	text     string
	priority int
}

type consoleFieldPresentation struct {
	label     string
	unit      string
	highlight bool
	priority  int
}

// Presentations are keyed by leaf name so grouped attributes retain their
// context while common project fields receive concise labels and units.
var consoleFieldPresentations = map[string]consoleFieldPresentation{
	"accepted":                {label: "Accepted"},
	"account_kind":            {label: "Account type"},
	"action_id":               {label: "Action ID"},
	"action_kind":             {label: "Action"},
	"address":                 {label: "Address"},
	"alert_key":               {label: "Alert"},
	"amount":                  {label: "Amount", unit: "USDC", highlight: true, priority: consoleFieldPriorityProminent},
	"attempt":                 {label: "Attempt"},
	"attempt_count":           {label: "Attempts"},
	"attempts":                {label: "Attempts"},
	"available":               {label: "Available", unit: "USDC", highlight: true, priority: consoleFieldPriorityAmount},
	"aws_region":              {label: "AWS region"},
	"balance_observed":        {label: "Balance observed"},
	"builder":                 {label: "Builder"},
	"builder_count":           {label: "Builders"},
	"claim_balance_visible":   {label: "Claim visible"},
	"claimable_usdc":          {label: "Claimable", unit: "USDC", highlight: true, priority: consoleFieldPriorityProminent},
	"coin":                    {label: "Coin"},
	"component":               {label: "Component"},
	"current_exists":          {label: "Saved run found"},
	"data_dir":                {label: "Data directory"},
	"decreased":               {label: "Decreased"},
	"dex":                     {label: "DEX"},
	"eligible":                {label: "Eligible"},
	"error":                   {label: "Error"},
	"evidence":                {label: "Evidence"},
	"fencing_token":           {label: "Fencing token"},
	"hold":                    {label: "Hold", unit: "USDC", highlight: true, priority: consoleFieldPriorityAmount},
	"last_available":          {label: "Last available", unit: "USDC", highlight: true, priority: consoleFieldPriorityAmount},
	"last_total":              {label: "Last total", unit: "USDC", highlight: true, priority: consoleFieldPriorityAmount},
	"next_run_at":             {label: "Next run"},
	"notification_kind":       {label: "Notification"},
	"operation":               {label: "Operation"},
	"outcome":                 {label: "Outcome"},
	"owner_id":                {label: "Owner ID"},
	"payout_total":            {label: "Payout", unit: "USDC", highlight: true, priority: consoleFieldPriorityProminent},
	"phase":                   {label: "Phase"},
	"primary_invalid":         {label: "Primary invalid"},
	"query_kind":              {label: "Query"},
	"raw_total":               {label: "Raw total", unit: "USDC", highlight: true, priority: consoleFieldPriorityAmount},
	"reason":                  {label: "Reason"},
	"recipient":               {label: "Recipient"},
	"recipient_count":         {label: "Recipients"},
	"record_count":            {label: "Records"},
	"recovered_from_backup":   {label: "Recovered from backup"},
	"rejected":                {label: "Rejected"},
	"requests_remaining":      {label: "Requests left"},
	"response":                {label: "Response"},
	"run_id":                  {label: "Run ID"},
	"run_on_start":            {label: "Run on start"},
	"settlement":              {label: "Settlement"},
	"settlement_total_before": {label: "Settlement before", unit: "USDC", highlight: true, priority: consoleFieldPriorityAmount},
	"signer_count":            {label: "Signers"},
	"sufficient":              {label: "Sufficient"},
	"threshold_usdc":          {label: "Threshold", unit: "USDC", highlight: true, priority: consoleFieldPriorityAmount},
	"total":                   {label: "Total", unit: "USDC", highlight: true, priority: consoleFieldPriorityAmount},
	"total_before":            {label: "Before", unit: "USDC", highlight: true, priority: consoleFieldPriorityAmount},
	"trigger":                 {label: "Trigger"},
	"unavailable_duration":    {label: "Unavailable"},
	"wait":                    {label: "Wait"},
	"warning_count":           {label: "Warnings"},
}

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
	var separator bool
	groupPrefix := strings.Join(h.groups, ".")
	fields := make([]consoleField, 0, len(h.attrs)+record.NumAttrs()+1)
	for _, attr := range h.attrs {
		h.appendAttr(&fields, &prefixValue, &separator, groupPrefix, attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		h.appendAttr(&fields, &prefixValue, &separator, groupPrefix, attr)
		return true
	})
	if h.addSource && record.PC != 0 {
		fields = append(fields, h.formatField("source", source(record.PC)))
	}

	message := record.Message
	if prefixValue != "" {
		message = prefixValue + " " + message
	}
	line := formatConsoleTime(when) + " " + h.formatLevel(record.Level) + " " + message
	if len(fields) > 0 {
		line += consoleFieldSeparator + strings.Join(orderConsoleFields(fields), consoleFieldSeparator)
	}
	line += "\n"
	if separator {
		line = "\n" + line
	}

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

func (h *consoleHandler) appendAttr(
	fields *[]consoleField,
	prefixValue *string,
	separator *bool,
	prefix string,
	attr slog.Attr,
) {
	if isEmptyAttr(attr) {
		return
	}
	// event is useful machine metadata in JSON output, but repeats the
	// already-human-readable message in console output.
	if attr.Key == consoleEventKey {
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
	if attr.Key == consoleSeparatorKey {
		// Like ConsolePrefix, the separator is presentation-only and only
		// applies at the top level. Drop nested occurrences entirely.
		if prefix == "" {
			value := attr.Value.Resolve()
			if value.Kind() == slog.KindBool {
				*separator = value.Bool()
			}
		}
		return
	}
	key := joinKey(prefix, attr.Key)
	if attr.Value.Kind() == slog.KindGroup {
		nextPrefix := key
		for _, child := range attr.Value.Group() {
			h.appendAttr(fields, prefixValue, separator, nextPrefix, child)
		}
		return
	}
	if attr.Key != "" && h.matcher.matches(key) {
		*fields = append(*fields, h.formatField(key, redactedValue))
		return
	}
	attr.Value = attr.Value.Resolve()
	if next, changed := h.matcher.redactKeyChanged(attr, key); changed {
		attr = next
	}
	if attr.Value.Kind() == slog.KindGroup {
		nextPrefix := joinKey(prefix, attr.Key)
		for _, child := range attr.Value.Group() {
			h.appendAttr(fields, prefixValue, separator, nextPrefix, child)
		}
		return
	}
	if attr.Key == "" {
		return
	}
	*fields = append(*fields, h.formatField(key, formatValue(attr.Value)))
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

func (h *consoleHandler) formatField(key, value string) consoleField {
	leaf := key
	if index := strings.LastIndexByte(key, '.'); index >= 0 {
		leaf = key[index+1:]
	}
	presentation := consoleFieldPresentations[leaf]
	label := formatConsoleLabel(key, presentation.label)
	value = formatConsoleFieldValue(value)
	if presentation.unit != "" && value != redactedValue && value != "<nil>" && value != `""` {
		value += " " + presentation.unit
	}
	field := label + ": " + value
	if h.color && presentation.highlight {
		field = ansiBoldCyan + field + ansiReset
	}
	return consoleField{text: field, priority: presentation.priority}
}

func orderConsoleFields(fields []consoleField) []string {
	ordered := make([]string, 0, len(fields))
	for _, priority := range []int{
		consoleFieldPriorityProminent,
		consoleFieldPriorityAmount,
		consoleFieldPriorityNormal,
	} {
		for _, field := range fields {
			if field.priority == priority {
				ordered = append(ordered, field.text)
			}
		}
	}
	return ordered
}

func formatConsoleLabel(key, leafLabel string) string {
	parts := strings.Split(key, ".")
	for index, part := range parts {
		if index == len(parts)-1 && leafLabel != "" {
			parts[index] = leafLabel
			continue
		}
		parts[index] = humanizeConsoleKey(part)
	}
	return strings.Join(parts, " / ")
}

func humanizeConsoleKey(key string) string {
	words := strings.Split(key, "_")
	for index, word := range words {
		switch strings.ToLower(word) {
		case "aws":
			words[index] = "AWS"
		case "ddb":
			words[index] = "DDB"
		case "dex":
			words[index] = "DEX"
		case "http":
			words[index] = "HTTP"
		case "id":
			words[index] = "ID"
		case "json":
			words[index] = "JSON"
		case "mysql":
			words[index] = "MySQL"
		case "pk":
			words[index] = "PK"
		case "url":
			words[index] = "URL"
		case "usdc":
			words[index] = "USDC"
		default:
			words[index] = strings.ToLower(word)
			if index == 0 && words[index] != "" {
				words[index] = strings.ToUpper(words[index][:1]) + words[index][1:]
			}
		}
	}
	return strings.Join(words, " ")
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
		if value.Bool() {
			return "yes"
		}
		return "no"
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

func formatConsoleFieldValue(value string) string {
	if value == "" {
		return `""`
	}
	if strings.ContainsRune(value, '│') {
		return strconv.Quote(value)
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return strconv.Quote(value)
		}
	}
	return value
}
