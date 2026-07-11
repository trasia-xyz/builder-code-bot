package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"hyperliquid-builder-code-bot/internal/secret"
)

func TestLoggerWritesStructuredJSON(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{
		Level:     LevelDebug,
		Output:    buf,
		Component: "oracle",
	})

	logger.Info(context.Background(), "published oracle",
		Dex("test"),
		Coin("test:TEST0"),
		OwnerID("node#run"),
		FencingToken(7),
		Duration("latency", 25*time.Millisecond),
	)

	event := decodeEvent(t, buf.String())
	if event["level"] != "INFO" {
		t.Fatalf("unexpected level: %#v", event["level"])
	}
	if event["msg"] != "published oracle" {
		t.Fatalf("unexpected msg: %#v", event["msg"])
	}
	if event["component"] != "oracle" || event["dex"] != "test" || event["coin"] != "test:TEST0" {
		t.Fatalf("missing structured fields: %#v", event)
	}
	if event["owner_id"] != "node#run" || event["fencing_token"].(float64) != 7 {
		t.Fatalf("missing leader fields: %#v", event)
	}
}

func TestLoggerWithAddsAttrsWithoutMutatingParent(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{Output: buf})
	child := logger.WithComponent("oracle").With(ActionID("action-1"))

	child.Info(context.Background(), "child log", Coin("TEST0"))
	logger.Info(context.Background(), "parent log")

	lines := logLines(buf.String())
	if len(lines) != 2 {
		t.Fatalf("expected two log lines, got %q", buf.String())
	}

	childEvent := decodeEvent(t, lines[0])
	if childEvent["component"] != "oracle" || childEvent["action_id"] != "action-1" || childEvent["coin"] != "TEST0" {
		t.Fatalf("missing child attrs: %#v", childEvent)
	}

	parentEvent := decodeEvent(t, lines[1])
	if _, ok := parentEvent["component"]; ok {
		t.Fatalf("parent logger must not inherit child attrs: %#v", parentEvent)
	}
	if parentEvent["msg"] != "parent log" {
		t.Fatalf("unexpected parent event: %#v", parentEvent)
	}
}

func TestLoggerUsesBackgroundWhenContextIsNil(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{Output: buf})

	var nilCtx context.Context
	logger.Info(nilCtx, "nil context")

	event := decodeEvent(t, buf.String())
	if event["msg"] != "nil context" {
		t.Fatalf("unexpected event: %#v", event)
	}
}

func TestSensitiveKeysAreRedacted(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{Output: buf})

	logger.Info(context.Background(), "loaded config",
		String("password", "secret-password"),
		String("signer_private_key", "0xabc"),
		String("access_token", "0xtok"),
		String("signature", "0xsig"),
		String("config_path", "/srv/config.toml"),
		String("safe", "visible"),
	)

	event := decodeEvent(t, buf.String())
	for _, key := range []string{"password", "signer_private_key", "access_token"} {
		if event[key] != redactedValue {
			t.Fatalf("expected %s to be redacted, got %#v", key, event[key])
		}
	}
	// Ambiguous keys were removed from the slimmed dictionary: they must pass
	// through so non-sensitive fields are not masked.
	visible := map[string]string{
		"signature":   "0xsig",
		"config_path": "/srv/config.toml",
		"safe":        "visible",
	}
	for key, want := range visible {
		if event[key] != want {
			t.Fatalf("expected %s to pass through as %q, got %#v", key, want, event[key])
		}
	}
}

func TestCustomSensitiveKeysAreNormalizedAndRedacted(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{
		Output:        buf,
		SensitiveKeys: []string{"  wallet  "},
	})

	logger.Info(context.Background(), "custom sensitive key",
		String("wallet_address", "0xabc"),
		String("note", "visible"),
	)

	event := decodeEvent(t, buf.String())
	if event["wallet_address"] != redactedValue {
		t.Fatalf("expected wallet_address to be redacted, got %#v", event["wallet_address"])
	}
	if event["note"] != "visible" {
		t.Fatalf("expected note to pass through, got %#v", event["note"])
	}
}

func TestLoggingNewRedactsSecretString(t *testing.T) {
	const raw = "super-secret-value"
	value := secret.NewString(raw)

	jsonBuf := new(bytes.Buffer)
	New(Config{Output: jsonBuf}).Info(context.Background(), "secret string", Any("note", value))

	event := decodeEvent(t, jsonBuf.String())
	if event["note"] != redactedValue {
		t.Fatalf("expected JSON SecretString to be redacted, got %#v", event["note"])
	}
	if strings.Contains(jsonBuf.String(), raw) {
		t.Fatalf("JSON log leaked raw secret: %q", jsonBuf.String())
	}

	consoleBuf := new(bytes.Buffer)
	New(Config{Format: FormatConsole, Output: consoleBuf}).Info(context.Background(), "secret string", Any("note", value))

	line := consoleBuf.String()
	if !strings.Contains(line, "note="+redactedValue) {
		t.Fatalf("expected console SecretString to be redacted, got %q", line)
	}
	if strings.Contains(line, raw) {
		t.Fatalf("console log leaked raw secret: %q", line)
	}
}

func TestGroupedSensitiveKeyPathsAreRedacted(t *testing.T) {
	jsonBuf := new(bytes.Buffer)
	jsonLogger := New(Config{
		Output:        jsonBuf,
		SensitiveKeys: []string{"wallet_address"},
	})
	jsonLogger.Info(context.Background(), "grouped",
		slog.Group("wallet", String("address", "0xsecret")),
		slog.Group("public", String("address", "visible")),
	)

	event := decodeEvent(t, jsonBuf.String())
	wallet, ok := event["wallet"].(map[string]any)
	if !ok {
		t.Fatalf("expected wallet group, got %#v", event["wallet"])
	}
	if wallet["address"] != redactedValue {
		t.Fatalf("expected grouped wallet address to be redacted, got %#v", wallet)
	}
	public, ok := event["public"].(map[string]any)
	if !ok || public["address"] != "visible" {
		t.Fatalf("expected public address to remain visible, got %#v", event["public"])
	}

	consoleBuf := new(bytes.Buffer)
	consoleLogger := New(Config{
		Format:        FormatConsole,
		Output:        consoleBuf,
		SensitiveKeys: []string{"wallet_address"},
	})
	consoleLogger.Info(context.Background(), "grouped",
		slog.Group("wallet", String("address", "0xsecret")),
		slog.Group("public", String("address", "visible")),
	)

	line := consoleBuf.String()
	if !strings.Contains(line, "wallet.address="+redactedValue) {
		t.Fatalf("expected grouped wallet address to be redacted in console line: %q", line)
	}
	if !strings.Contains(line, "public.address=visible") {
		t.Fatalf("expected public address to remain visible in console line: %q", line)
	}
	if strings.Contains(line, "0xsecret") {
		t.Fatalf("sensitive grouped value leaked in console line: %q", line)
	}
}

func TestWithRedactsGroupedSensitiveKeyPaths(t *testing.T) {
	jsonBuf := new(bytes.Buffer)
	jsonLogger := New(Config{
		Output:        jsonBuf,
		SensitiveKeys: []string{"wallet_address"},
	}).With(
		slog.Group("wallet", String("address", "0xsecret")),
		slog.Group("public", String("address", "visible")),
	)
	jsonLogger.Info(context.Background(), "with grouped")

	event := decodeEvent(t, jsonBuf.String())
	wallet, ok := event["wallet"].(map[string]any)
	if !ok {
		t.Fatalf("expected wallet group, got %#v", event["wallet"])
	}
	if wallet["address"] != redactedValue {
		t.Fatalf("expected grouped wallet address from With to be redacted, got %#v", wallet)
	}
	public, ok := event["public"].(map[string]any)
	if !ok || public["address"] != "visible" {
		t.Fatalf("expected public address from With to remain visible, got %#v", event["public"])
	}

	consoleBuf := new(bytes.Buffer)
	consoleLogger := New(Config{
		Format:        FormatConsole,
		Output:        consoleBuf,
		SensitiveKeys: []string{"wallet_address"},
	}).With(
		slog.Group("wallet", String("address", "0xsecret")),
		slog.Group("public", String("address", "visible")),
	)
	consoleLogger.Info(context.Background(), "with grouped")

	line := consoleBuf.String()
	if !strings.Contains(line, "wallet.address="+redactedValue) {
		t.Fatalf("expected grouped wallet address from With to be redacted in console line: %q", line)
	}
	if !strings.Contains(line, "public.address=visible") {
		t.Fatalf("expected public address from With to remain visible in console line: %q", line)
	}
	if strings.Contains(line, "0xsecret") {
		t.Fatalf("sensitive grouped value from With leaked in console line: %q", line)
	}
}

func TestNilErrorAttrIsOmitted(t *testing.T) {
	jsonBuf := new(bytes.Buffer)
	jsonLogger := New(Config{Output: jsonBuf})
	jsonLogger.Info(context.Background(), "nil error", Err(nil), String("safe", "visible"))

	event := decodeEvent(t, jsonBuf.String())
	if _, ok := event["error"]; ok {
		t.Fatalf("nil error attr should be omitted from JSON event: %#v", event)
	}
	if event["safe"] != "visible" {
		t.Fatalf("expected safe field to pass through, got %#v", event["safe"])
	}

	consoleBuf := new(bytes.Buffer)
	consoleLogger := New(Config{Format: FormatConsole, Output: consoleBuf})
	consoleLogger.Info(context.Background(), "nil error", Err(nil), String("safe", "visible"))

	line := consoleBuf.String()
	if strings.Contains(line, "error=") {
		t.Fatalf("nil error attr should be omitted from console line: %q", line)
	}
	if !strings.Contains(line, "safe=visible") {
		t.Fatalf("expected safe field in console line: %q", line)
	}
}

func TestRedactedValueIsReadable(t *testing.T) {
	if redactedValue != "[MASKED]" {
		t.Fatalf("unexpected redacted value: %q", redactedValue)
	}
}

func TestJSONSourceUsesRepoRelativePath(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{
		AddSource: true,
		Output:    buf,
	})

	logger.Info(context.Background(), "with source")

	event := decodeEvent(t, buf.String())
	source, ok := event["source"].(map[string]any)
	if !ok {
		t.Fatalf("expected source object, got %#v", event["source"])
	}
	file, ok := source["file"].(string)
	if !ok {
		t.Fatalf("expected source.file string, got %#v", source["file"])
	}
	assertRepoRelativeSourceFile(t, file)
}

func TestInvalidLevelFallsBackToInfo(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{
		Level:  "trace",
		Output: buf,
	})

	logger.Debug(context.Background(), "ignored")
	logger.Info(context.Background(), "kept")

	lines := logLines(buf.String())
	if len(lines) != 1 {
		t.Fatalf("expected one log line, got %q", buf.String())
	}
	event := decodeEvent(t, lines[0])
	if event["msg"] != "kept" {
		t.Fatalf("unexpected event: %#v", event)
	}
}

func TestLevelFiltering(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{Level: LevelWarn, Output: buf})

	logger.Info(context.Background(), "ignored")
	logger.Warn(context.Background(), "kept")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected one log line, got %q", buf.String())
	}
	event := decodeEvent(t, lines[0])
	if event["msg"] != "kept" {
		t.Fatalf("unexpected event: %#v", event)
	}
}

func TestLevelNameFiltering(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{Level: LevelError, Output: buf})

	logger.Warn(context.Background(), "ignored")
	logger.Error(context.Background(), "kept")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected one log line, got %q", buf.String())
	}
	event := decodeEvent(t, lines[0])
	if event["msg"] != "kept" {
		t.Fatalf("unexpected event: %#v", event)
	}
}

func TestGlobalLoggingFunctionsUseDefaultLogger(t *testing.T) {
	buf := new(bytes.Buffer)
	previous := Default()
	t.Cleanup(func() { SetDefault(previous) })

	SetDefault(New(Config{Level: LevelDebug, Output: buf}))

	Debug(context.Background(), "debug")
	Info(context.Background(), "info")
	Warn(context.Background(), "warn")
	Error(context.Background(), "error")
	Debugf("debug %d", 2)
	Infof("info %d", 2)
	Warnf("warn %d", 2)
	Errorf("error %d", 2)

	lines := logLines(buf.String())
	if len(lines) != 8 {
		t.Fatalf("expected eight log lines, got %q", buf.String())
	}
	wantMessages := []string{"debug", "info", "warn", "error", "debug 2", "info 2", "warn 2", "error 2"}
	for i, want := range wantMessages {
		event := decodeEvent(t, lines[i])
		if event["msg"] != want {
			t.Fatalf("unexpected message at %d: got %#v want %q", i, event["msg"], want)
		}
	}
}

func TestConsoleFormatWritesHumanReadableLine(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{
		Format:    FormatConsole,
		Level:     LevelDebug,
		Color:     ColorAuto,
		Output:    buf,
		Component: "oracle",
	})

	logger.Debug(context.Background(), "published oracle",
		String("password", "secret"),
		Int("coin_count", 12),
	)

	line := buf.String()
	for _, want := range []string{
		"DEBUG",
		"published oracle",
		"component=oracle",
		"coin_count=12",
		"password=" + redactedValue,
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("expected %q in console log line %q", want, line)
		}
	}
	if strings.Contains(line, "\x1b[") {
		t.Fatalf("buffer output must not be colorized, got %q", line)
	}
}

func TestConsolePrefixPrefixesMessage(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{Format: FormatConsole, Output: buf})

	logger.Info(context.Background(), "leader_status",
		ConsolePrefix("*"),
		Dex("asia"),
	)

	line := buf.String()
	if !strings.Contains(line, "INFO  * leader_status") {
		t.Fatalf("prefix was not placed before message: %q", line)
	}
	if strings.Contains(line, consolePrefixKey) {
		t.Fatalf("console prefix must not be logged as key=value: %q", line)
	}
	if !strings.Contains(line, "dex=asia") {
		t.Fatalf("expected ordinary attrs to remain: %q", line)
	}
}

func TestJSONDropsConsolePrefix(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{Format: FormatJSON, Output: buf})

	logger.Info(context.Background(), "leader_status",
		ConsolePrefix("*"),
		Dex("asia"),
	)

	event := decodeEvent(t, buf.String())
	if event["msg"] != "leader_status" {
		t.Fatalf("unexpected msg: %#v", event["msg"])
	}
	if _, ok := event[consolePrefixKey]; ok {
		t.Fatalf("console prefix leaked into json: %#v", event)
	}
	if event["dex"] != "asia" {
		t.Fatalf("ordinary attrs missing from json: %#v", event)
	}
}

func TestConsoleFormatExpandsGroupsAndEscapesValues(t *testing.T) {
	buf := new(bytes.Buffer)
	handler := newConsoleHandler(buf, slog.LevelDebug, false, false, []string{"secret"})
	logger := slog.New(handler).With("component", "leader").WithGroup("lease")

	logger.Info("renewed",
		slog.Group("ddb",
			slog.String("pk", "dex#prod"),
			slog.String("secret", "hidden"),
		),
		slog.String("note", "hello world"),
		slog.String("empty", ""),
	)

	line := buf.String()
	for _, want := range []string{
		"component=leader",
		"lease.ddb.pk=dex#prod",
		"lease.ddb.secret=" + redactedValue,
		`lease.note="hello world"`,
		`lease.empty=""`,
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("expected %q in console log line %q", want, line)
		}
	}
}

func TestConsoleFormatValues(t *testing.T) {
	when := time.Date(2026, 6, 11, 10, 12, 13, 123_456_000, time.UTC)
	tests := []struct {
		name string
		attr slog.Attr
		want string
	}{
		{name: "bool", attr: Bool("enabled", true), want: "enabled=true"},
		{name: "int64", attr: Int64("nonce", 1781153533123), want: "nonce=1781153533123"},
		{name: "uint64", attr: Uint64("height", 99), want: "height=99"},
		{name: "duration", attr: Duration("latency", 25*time.Millisecond), want: "latency=25ms"},
		{name: "time", attr: Time("block_time", when), want: "block_time=2026-06-11T10:12:13.123456Z"},
		{name: "any nil", attr: Any("payload", nil), want: "payload=<nil>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := new(bytes.Buffer)
			logger := New(Config{Format: FormatConsole, Output: buf})

			logger.Info(context.Background(), "value", tt.attr)

			if !strings.Contains(buf.String(), tt.want) {
				t.Fatalf("expected %q in console log line %q", tt.want, buf.String())
			}
		})
	}
}

func TestAnyMapAttrsRemainLoggable(t *testing.T) {
	jsonBuf := new(bytes.Buffer)
	New(Config{Output: jsonBuf}).Info(context.Background(), "map payload", Any("payload", map[string]int{"n": 1}))

	event := decodeEvent(t, jsonBuf.String())
	payload, ok := event["payload"].(map[string]any)
	if !ok {
		t.Fatalf("expected JSON payload object, got %#v", event["payload"])
	}
	if payload["n"] != float64(1) {
		t.Fatalf("unexpected JSON payload: %#v", payload)
	}

	consoleBuf := new(bytes.Buffer)
	New(Config{Format: FormatConsole, Output: consoleBuf}).Info(context.Background(), "map payload", Any("payload", map[string]int{"n": 1}))
	if !strings.Contains(consoleBuf.String(), `payload="{\"n\":1}"`) {
		t.Fatalf("expected escaped map payload in console line: %q", consoleBuf.String())
	}
}

func TestConsoleHandlerWithEmptyGroupIsNoop(t *testing.T) {
	handler := newConsoleHandler(new(bytes.Buffer), slog.LevelDebug, false, false, nil)
	if got := handler.WithGroup(""); got != handler {
		t.Fatalf("empty group should return the same handler")
	}
}

func TestFormatAny(t *testing.T) {
	if got := formatAny(testStringer("value")); got != "stringer:value" {
		t.Fatalf("unexpected stringer format: %q", got)
	}
	if got := formatAny(map[string]int{"n": 1}); got != `{"n":1}` {
		t.Fatalf("unexpected json format: %q", got)
	}
	if got := formatAny(func() {}); got == "" {
		t.Fatal("fallback format must not be empty")
	}
}

func TestConsoleFormatPadsLevelToFiveCharacters(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{
		Format: FormatConsole,
		Output: buf,
	})

	logger.Info(context.Background(), "info message")
	logger.Error(context.Background(), "error message")

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two lines, got %q", buf.String())
	}
	if !strings.Contains(lines[0], " INFO  info message") {
		t.Fatalf("expected padded INFO level, got %q", lines[0])
	}
	if !strings.Contains(lines[1], " ERROR error message") {
		t.Fatalf("expected five-character ERROR level, got %q", lines[1])
	}
}

func TestConsoleFormatAddsRepoRelativeSourceWhenEnabled(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{
		Format:    FormatConsole,
		AddSource: true,
		Output:    buf,
	})

	logger.Info(context.Background(), "with source")

	line := buf.String()
	if !strings.Contains(line, "source=internal/logging/logging_test.go:") {
		t.Fatalf("expected repo-relative source in console line %q", line)
	}
	if strings.Contains(line, "/builder-code/") {
		t.Fatalf("source must not contain absolute project path: %q", line)
	}
}

func TestConsoleFormatUsesFixedWidthTimestamp(t *testing.T) {
	buf := new(bytes.Buffer)
	handler := newConsoleHandler(buf, slog.LevelDebug, false, false, nil)

	first := slog.NewRecord(
		time.Date(2026, 6, 11, 10, 12, 13, 123_000_000, time.UTC),
		slog.LevelInfo,
		"first",
		0,
	)
	second := slog.NewRecord(
		time.Date(2026, 6, 11, 10, 12, 14, 0, time.UTC),
		slog.LevelInfo,
		"second",
		0,
	)

	if err := handler.Handle(context.Background(), first); err != nil {
		t.Fatalf("handle first record: %v", err)
	}
	if err := handler.Handle(context.Background(), second); err != nil {
		t.Fatalf("handle second record: %v", err)
	}

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two lines, got %q", buf.String())
	}
	if !strings.HasPrefix(lines[0], "2026-06-11T10:12:13.123000Z INFO  first") {
		t.Fatalf("unexpected first timestamp: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "2026-06-11T10:12:14.000000Z INFO  second") {
		t.Fatalf("unexpected second timestamp: %q", lines[1])
	}
}

func TestConsoleHandlerReturnsWriteError(t *testing.T) {
	wantErr := errors.New("write failed")
	handler := newConsoleHandler(errorWriter{err: wantErr}, slog.LevelDebug, false, false, nil)
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "message", 0)

	err := handler.Handle(context.Background(), record)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected write error %v, got %v", wantErr, err)
	}
}

func TestLoggerReportsHandleErrorToStderr(t *testing.T) {
	resetLogHandleErrorReporter(t, 30*time.Second)
	logger := New(Config{
		Format: FormatConsole,
		Output: errorWriter{err: errors.New("write failed")},
	})

	stderr := captureStderr(t, func() {
		logger.Info(context.Background(), "write_failure", String("password", "secret"))
	})

	for _, want := range []string{
		"logging: failed to write log record",
		"level=INFO",
		`msg="write_failure"`,
		"error=write failed",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("expected %q in stderr diagnostic %q", want, stderr)
		}
	}
	if strings.Contains(stderr, "password") || strings.Contains(stderr, "secret") {
		t.Fatalf("stderr diagnostic must not include attrs: %q", stderr)
	}
}

func TestHandleErrorReporterRateLimitsAndReportsSuppressed(t *testing.T) {
	reporter := handleErrorReporter{interval: time.Minute}
	err := errors.New("write failed")
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)

	stderr := captureStderr(t, func() {
		reporter.report(now, slog.LevelInfo, "first", err)
		reporter.report(now.Add(time.Second), slog.LevelInfo, "second", err)
		reporter.report(now.Add(2*time.Second), slog.LevelInfo, "third", err)
		reporter.report(now.Add(time.Minute), slog.LevelInfo, "fourth", err)
	})

	if count := strings.Count(stderr, "logging: failed to write log record"); count != 2 {
		t.Fatalf("expected two stderr diagnostics, got %d in %q", count, stderr)
	}
	if !strings.Contains(stderr, `msg="first"`) {
		t.Fatalf("expected first diagnostic, got %q", stderr)
	}
	if !strings.Contains(stderr, `msg="fourth"`) || !strings.Contains(stderr, "suppressed=2") {
		t.Fatalf("expected suppressed count on second diagnostic, got %q", stderr)
	}
	if strings.Contains(stderr, `msg="second"`) || strings.Contains(stderr, `msg="third"`) {
		t.Fatalf("rate-limited diagnostics should not be emitted individually: %q", stderr)
	}
}

func TestConsoleHandlerColorizesLevels(t *testing.T) {
	handler := newConsoleHandler(new(bytes.Buffer), slog.LevelDebug, false, true, nil).(*consoleHandler)
	tests := []struct {
		level slog.Level
		want  string
	}{
		{level: slog.LevelDebug, want: ansiCyan + "DEBUG" + ansiReset},
		{level: slog.LevelInfo, want: ansiGreen + "INFO " + ansiReset},
		{level: slog.LevelWarn, want: ansiYellow + "WARN " + ansiReset},
		{level: slog.LevelError, want: ansiRed + "ERROR" + ansiReset},
	}

	for _, tt := range tests {
		t.Run(levelName(tt.level), func(t *testing.T) {
			got := handler.formatLevel(tt.level)
			if !strings.Contains(got, tt.want) {
				t.Fatalf("unexpected colorized level: got %q want containing %q", got, tt.want)
			}
		})
	}
}

func TestConsoleSourceUsesRepoRelativePath(t *testing.T) {
	pc, _, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller failed")
	}

	got := source(pc)
	if !strings.HasPrefix(got, "internal/logging/logging_test.go:") {
		t.Fatalf("unexpected source path: %q", got)
	}
}

func TestTrimSourceFileRemovesProjectRoot(t *testing.T) {
	got := trimSourceFile(filepath.Join("/tmp/build", "hyperliquid-builder-code-bot", "internal", "logging", "logging.go"))
	if got != "internal/logging/logging.go" {
		t.Fatalf("unexpected trimmed source path: %q", got)
	}
}

func TestModuleDirectoryName(t *testing.T) {
	tests := []struct {
		modulePath string
		want       string
	}{
		{modulePath: "hyperliquid-builder-code-bot", want: "hyperliquid-builder-code-bot"},
		{modulePath: "github.com/org/hyperliquid-builder-code-bot", want: "hyperliquid-builder-code-bot"},
		{modulePath: "  github.com/org/service  ", want: "service"},
		{modulePath: "", want: ""},
		{modulePath: "command-line-arguments", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.modulePath, func(t *testing.T) {
			if got := moduleDirectoryName(tt.modulePath); got != tt.want {
				t.Fatalf("unexpected module directory name: got %q want %q", got, tt.want)
			}
		})
	}
}

func TestTrimSourceFileFallbacks(t *testing.T) {
	tests := []struct {
		name string
		file string
		want string
	}{
		{name: "empty", file: "", want: ""},
		{name: "already relative with repo", file: "hyperliquid-builder-code-bot/internal/logging/logging.go", want: "internal/logging/logging.go"},
		{name: "unrelated absolute", file: "/tmp/other/logging.go", want: "/tmp/other/logging.go"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := trimSourceFile(tt.file)
			if got != tt.want {
				t.Fatalf("unexpected trimmed source path: got %q want %q", got, tt.want)
			}
		})
	}
}

func TestSourceRootDirectoryDoesNotCacheMiss(t *testing.T) {
	resetSourceRootCache(t, "")
	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalWD); err != nil {
			t.Fatalf("restore working directory: %v", err)
		}
	})

	outside := t.TempDir()
	if err := os.Chdir(outside); err != nil {
		t.Fatalf("chdir outside repo: %v", err)
	}
	if got := sourceRootDirectory(); got != "" {
		t.Fatalf("expected missing source root outside repo, got %q", got)
	}

	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repoRoot, 0o755); err != nil {
		t.Fatalf("create repo root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "go.mod"), []byte("module test\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	subdir := filepath.Join(repoRoot, "cmd")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatalf("create subdir: %v", err)
	}
	if err := os.Chdir(subdir); err != nil {
		t.Fatalf("chdir inside repo: %v", err)
	}

	got := sourceRootDirectory()
	want, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("expected source root %q after earlier miss, got %q", want, got)
	}
}

func TestTrimSourceAttr(t *testing.T) {
	attr := trimSourceAttr(slog.Any(slog.SourceKey, &slog.Source{
		File: "/tmp/build/hyperliquid-builder-code-bot/internal/logging/logging.go",
		Line: 12,
	}))

	source, ok := attr.Value.Any().(*slog.Source)
	if !ok {
		t.Fatalf("expected source attr, got %#v", attr.Value.Any())
	}
	if source.File != "internal/logging/logging.go" || source.Line != 12 {
		t.Fatalf("unexpected source attr: %#v", source)
	}

	nilAttr := trimSourceAttr(slog.Any(slog.SourceKey, (*slog.Source)(nil)))
	if nilAttr.Value.Any() != (*slog.Source)(nil) {
		t.Fatalf("nil source attr should pass through, got %#v", nilAttr.Value.Any())
	}

	stringAttr := trimSourceAttr(slog.String(slog.SourceKey, "source"))
	if stringAttr.Value.String() != "source" {
		t.Fatalf("non-source attr should pass through, got %#v", stringAttr)
	}
}

func TestDefaultLoggerCanBeReplaced(t *testing.T) {
	buf := new(bytes.Buffer)
	previous := Default()
	t.Cleanup(func() { SetDefault(previous) })

	SetDefault(New(Config{Output: buf}))
	Info(context.Background(), "hello", String("component", "test"))

	event := decodeEvent(t, buf.String())
	if event["msg"] != "hello" || event["component"] != "test" {
		t.Fatalf("unexpected event: %#v", event)
	}
}

func TestAttrHelpers(t *testing.T) {
	when := time.Date(2026, 6, 11, 10, 12, 13, 0, time.FixedZone("CST", 8*60*60))
	err := errors.New("failed")

	tests := []struct {
		name string
		attr slog.Attr
		key  string
		want any
	}{
		{name: "err value", attr: Err(err), key: "error", want: "failed"},
		{name: "bool", attr: Bool("enabled", true), key: "enabled", want: true},
		{name: "int64", attr: Int64("nonce", 7), key: "nonce", want: int64(7)},
		{name: "uint64", attr: Uint64("height", 8), key: "height", want: uint64(8)},
		{name: "time utc", attr: Time("ts", when), key: "ts", want: when.UTC()},
		{name: "any", attr: Any("payload", map[string]int{"n": 1}), key: "payload", want: map[string]int{"n": 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.attr.Key != tt.key {
				t.Fatalf("unexpected attr key: got %q want %q", tt.attr.Key, tt.key)
			}
			if !reflect.DeepEqual(tt.attr.Value.Any(), tt.want) {
				t.Fatalf("unexpected attr value: got %#v want %#v", tt.attr.Value.Any(), tt.want)
			}
		})
	}

	if got := Err(nil); !got.Equal(slog.Attr{}) {
		t.Fatalf("nil error should return empty attr, got %#v", got)
	}
}

func decodeEvent(t *testing.T, line string) map[string]any {
	t.Helper()
	var event map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &event); err != nil {
		t.Fatalf("decode event: %v\nline=%s", err, line)
	}
	return event
}

func assertRepoRelativeSourceFile(t *testing.T, file string) {
	t.Helper()
	if file == "" {
		t.Fatal("source file must not be empty")
	}
	if filepath.IsAbs(file) || strings.HasPrefix(file, "../") || strings.Contains(file, "/builder-code/") {
		t.Fatalf("source file must be repo-relative, got %q", file)
	}
}

func logLines(output string) []string {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil
	}
	return strings.Split(output, "\n")
}

type errorWriter struct {
	err error
}

func (w errorWriter) Write(_ []byte) (int, error) {
	return 0, w.err
}

type testStringer string

func (s testStringer) String() string {
	return "stringer:" + string(s)
}

func TestZeroValueLoggerFallsBackToDefault(t *testing.T) {
	buf := new(bytes.Buffer)
	previous := Default()
	t.Cleanup(func() { SetDefault(previous) })
	SetDefault(New(Config{Output: buf}))

	var zero Logger
	// A Logger that was never built via New carries a nil *slog.Logger; calling
	// it must not panic and should route through the installed default.
	zero.Info(context.Background(), "via zero value", String("component", "zero"))

	event := decodeEvent(t, buf.String())
	if event["msg"] != "via zero value" || event["component"] != "zero" {
		t.Fatalf("zero-value logger did not fall back to default: %#v", event)
	}
}

func TestZeroValueLoggerWithDoesNotPanic(t *testing.T) {
	buf := new(bytes.Buffer)
	previous := Default()
	t.Cleanup(func() { SetDefault(previous) })
	SetDefault(New(Config{Output: buf}))

	var zero Logger
	child := zero.With(Component("child"))
	child.Info(context.Background(), "child via zero")

	event := decodeEvent(t, buf.String())
	if event["component"] != "child" || event["msg"] != "child via zero" {
		t.Fatalf("zero-value With did not fall back to default: %#v", event)
	}
}

func TestSetDefaultIgnoresUninitializedLogger(t *testing.T) {
	buf := new(bytes.Buffer)
	previous := Default()
	t.Cleanup(func() { SetDefault(previous) })
	SetDefault(New(Config{Output: buf}))

	stderr := captureStderr(t, func() {
		SetDefault(Logger{})
	})
	if !strings.Contains(stderr, "uninitialized Logger") {
		t.Fatalf("expected a stderr warning about the uninitialized logger, got %q", stderr)
	}

	// The previous default must remain installed and usable.
	Info(context.Background(), "still alive")
	event := decodeEvent(t, buf.String())
	if event["msg"] != "still alive" {
		t.Fatalf("default logger was clobbered by an uninitialized logger: %#v", event)
	}
}

func TestLogRecoversFromPanickingValue(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{Format: FormatConsole, Output: buf})

	stderr := captureStderr(t, func() {
		// A value whose String() panics must never take the process down.
		logger.Info(context.Background(), "panic attr", Any("payload", panicStringer{}))
	})
	if !strings.Contains(stderr, "recovered panic") {
		t.Fatalf("expected a recovery message on stderr, got %q", stderr)
	}
}

func TestLogRecoversAtPackageLevel(t *testing.T) {
	buf := new(bytes.Buffer)
	previous := Default()
	t.Cleanup(func() { SetDefault(previous) })
	SetDefault(New(Config{Format: FormatConsole, Output: buf}))

	stderr := captureStderr(t, func() {
		// The package-level helpers go through the same guarded log() path.
		Info(context.Background(), "panic attr", Any("payload", panicStringer{}))
	})
	if !strings.Contains(stderr, "recovered panic") {
		t.Fatalf("expected a recovery message on stderr, got %q", stderr)
	}
}

func TestSourceOmittedWhenAddSourceDisabled(t *testing.T) {
	jsonBuf := new(bytes.Buffer)
	New(Config{Output: jsonBuf}).Info(context.Background(), "no source")
	event := decodeEvent(t, jsonBuf.String())
	if _, ok := event["source"]; ok {
		t.Fatalf("source must be omitted when AddSource is false: %#v", event)
	}

	consoleBuf := new(bytes.Buffer)
	New(Config{Format: FormatConsole, Output: consoleBuf}).Info(context.Background(), "no source")
	if strings.Contains(consoleBuf.String(), "source=") {
		t.Fatalf("source must be omitted from console line: %q", consoleBuf.String())
	}
}

func TestWithPreservesAddSource(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{AddSource: true, Output: buf}).With(Component("oracle"))

	logger.Info(context.Background(), "with source after With")

	event := decodeEvent(t, buf.String())
	if _, ok := event["source"]; !ok {
		t.Fatalf("AddSource must survive With(): %#v", event)
	}
	if event["component"] != "oracle" {
		t.Fatalf("With() attrs missing: %#v", event)
	}
}

func TestConsolePrefixIgnoredInsideGroup(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{Format: FormatConsole, Output: buf})

	logger.Info(context.Background(), "leader_status",
		slog.Group("ctx", ConsolePrefix("*"), Dex("asia")),
	)

	line := buf.String()
	if strings.Contains(line, "* leader_status") {
		t.Fatalf("nested ConsolePrefix must not prefix the message: %q", line)
	}
	if strings.Contains(line, consolePrefixKey) {
		t.Fatalf("nested ConsolePrefix must not leak as a field: %q", line)
	}
	if !strings.Contains(line, "ctx.dex=asia") {
		t.Fatalf("ordinary group attrs must remain: %q", line)
	}
}

func TestJSONDropsConsolePrefixInsideGroup(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{Format: FormatJSON, Output: buf})

	logger.Info(context.Background(), "leader_status",
		slog.Group("ctx", ConsolePrefix("*"), Dex("asia")),
	)

	event := decodeEvent(t, buf.String())
	group, ok := event["ctx"].(map[string]any)
	if !ok {
		t.Fatalf("expected ctx group object, got %#v", event["ctx"])
	}
	if _, ok := group[consolePrefixKey]; ok {
		t.Fatalf("nested ConsolePrefix leaked into JSON: %#v", group)
	}
	if group["dex"] != "asia" {
		t.Fatalf("ordinary group attrs missing from JSON: %#v", group)
	}
}

func TestConsoleRedactsSensitiveGroupBeforeResolvingChildren(t *testing.T) {
	buf := new(bytes.Buffer)
	logger := New(Config{Format: FormatConsole, Output: buf})

	stderr := captureStderr(t, func() {
		logger.Info(context.Background(), "group secret",
			slog.Group("private_key", Any("value", panicStringer{})),
		)
	})
	if strings.Contains(stderr, "recovered panic") {
		t.Fatalf("sensitive group child should not have been resolved, stderr=%q", stderr)
	}

	line := buf.String()
	if !strings.Contains(line, "private_key.value="+redactedValue) {
		t.Fatalf("expected sensitive group child to be redacted: %q", line)
	}
	if strings.Contains(line, "boom from String") || strings.Contains(line, "{}") {
		t.Fatalf("sensitive group child value should not be formatted: %q", line)
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	done := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()

	fn()

	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

func resetLogHandleErrorReporter(t *testing.T, interval time.Duration) {
	t.Helper()
	logHandleErrorReporter.mu.Lock()
	previousInterval := logHandleErrorReporter.interval
	previousLastReport := logHandleErrorReporter.lastReport
	previousSuppressed := logHandleErrorReporter.suppressed
	logHandleErrorReporter.interval = interval
	logHandleErrorReporter.lastReport = time.Time{}
	logHandleErrorReporter.suppressed = 0
	logHandleErrorReporter.mu.Unlock()

	t.Cleanup(func() {
		logHandleErrorReporter.mu.Lock()
		logHandleErrorReporter.interval = previousInterval
		logHandleErrorReporter.lastReport = previousLastReport
		logHandleErrorReporter.suppressed = previousSuppressed
		logHandleErrorReporter.mu.Unlock()
	})
}

func resetSourceRootCache(t *testing.T, root string) {
	t.Helper()
	sourceRootMu.Lock()
	previous := sourceRootDir
	sourceRootDir = root
	sourceRootMu.Unlock()

	t.Cleanup(func() {
		sourceRootMu.Lock()
		sourceRootDir = previous
		sourceRootMu.Unlock()
	})
}

type panicStringer struct{}

func (panicStringer) String() string { panic("boom from String") }
