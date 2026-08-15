package logging

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSafeErrorTextPreservesOrdinaryCause(t *testing.T) {
	want := "query claimable: dial tcp 127.0.0.1:443: connection refused"
	if got := SafeErrorText(errors.New(want)); got != want {
		t.Fatalf("SafeErrorText() = %q, want %q", got, want)
	}
}

func TestSafeErrorTextRedactsCredentialsAndPreservesDiagnosis(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		secret string
	}{
		{name: "password", input: "open database password=hunter2; connection refused", secret: "hunter2"},
		{name: "passwd", input: "connect passwd='hidden value', access denied", secret: "hidden value"},
		{name: "pwd", input: `connect pwd="hidden value": access denied`, secret: "hidden value"},
		{name: "private key", input: "sign private_key=0xdeadbeef: signer unavailable", secret: "0xdeadbeef"},
		{name: "api key", input: "request api_key=key-123; forbidden", secret: "key-123"},
		{name: "mnemonic", input: "load mnemonic='word one word two'; invalid account", secret: "word one word two"},
		{name: "passphrase", input: "decrypt passphrase=correct-horse; invalid ciphertext", secret: "correct-horse"},
		{name: "signing key", input: "sign signing_key=0xfeedface; unavailable", secret: "0xfeedface"},
		{name: "prefixed secret", input: `decode {"client_secret":"top-secret"}: invalid response`, secret: "top-secret"},
		{name: "token", input: "request https://api.example.test/path?access_token=abc123&limit=1: timeout", secret: "abc123"},
		{name: "url userinfo", input: "dial mysql://db-user:db-pass@localhost/app: connection refused", secret: "db-user:db-pass"},
		{name: "go mysql DSN", input: "dial db-user:p@ss@tcp(localhost:3306)/app: connection refused", secret: "db-user:p@ss"},
		{name: "bearer", input: "request Authorization: Bearer eyJhbGciOiJub25l: unauthorized", secret: "eyJhbGciOiJub25l"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SafeErrorText(errors.New(tt.input))
			if strings.Contains(got, tt.secret) {
				t.Fatalf("SafeErrorText() leaked %q in %q", tt.secret, got)
			}
			if !strings.Contains(got, redactedValue) {
				t.Fatalf("SafeErrorText() = %q, want redaction marker", got)
			}
			if !strings.Contains(got, strings.Fields(tt.input)[0]) {
				t.Fatalf("SafeErrorText() removed diagnostic context: %q", got)
			}
		})
	}
}

func TestErrUsesSafeErrorText(t *testing.T) {
	attr := Err(errors.New("submit failed token=do-not-log; upstream rejected request"))
	if attr.Key != "error" {
		t.Fatalf("Err() key = %q, want error", attr.Key)
	}
	got := attr.Value.String()
	if strings.Contains(got, "do-not-log") || !strings.Contains(got, "upstream rejected request") {
		t.Fatalf("Err() value = %q", got)
	}
}

func TestLoggerSanitizesStringAndErrorValuesUnderErrorKeys(t *testing.T) {
	var output strings.Builder
	logger := New(Config{Format: FormatJSON, Level: LevelDebug, Output: &output})
	logger.Error(context.Background(), "failed",
		String("submit_error", "private_key=raw-key; request rejected"),
		Any("nested_error", errors.New("token=raw-token; gateway timeout")),
	)
	logs := output.String()
	for _, secret := range []string{"raw-key", "raw-token"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logger leaked %q: %s", secret, logs)
		}
	}
	for _, cause := range []string{"request rejected", "gateway timeout"} {
		if !strings.Contains(logs, cause) {
			t.Fatalf("logger removed %q: %s", cause, logs)
		}
	}
}

func TestSafeErrorTextNil(t *testing.T) {
	if got := SafeErrorText(nil); got != "" {
		t.Fatalf("SafeErrorText(nil) = %q, want empty", got)
	}
}

func TestSafeDiagnosticTextRedactsBearerAndURLUserinfo(t *testing.T) {
	got := SafeDiagnosticText(
		"upstream Authorization: Basic basic-secret; Cookie: session=cookie-secret\n" +
			"dial https://user:pass@example.test",
	)
	for _, secret := range []string{"basic-secret", "cookie-secret", "user:pass"} {
		if strings.Contains(got, secret) {
			t.Fatalf("SafeDiagnosticText() leaked %q in %q", secret, got)
		}
	}
	if !strings.Contains(got, "upstream") || !strings.Contains(got, redactedValue) {
		t.Fatalf("SafeDiagnosticText() lost diagnostic context: %q", got)
	}
}

func TestSafeDiagnosticTextRedactsUnquotedMultiwordSecret(t *testing.T) {
	got := SafeDiagnosticText("decrypt mnemonic=word1 word2 word3; invalid account")
	for _, secret := range []string{"word1", "word2", "word3"} {
		if strings.Contains(got, secret) {
			t.Fatalf("SafeDiagnosticText() leaked %q in %q", secret, got)
		}
	}
	if !strings.Contains(got, "invalid account") {
		t.Fatalf("SafeDiagnosticText() lost diagnosis: %q", got)
	}
}

func TestSafeDiagnosticExcerptIsBoundedAndValidUTF8(t *testing.T) {
	got := SafeDiagnosticExcerpt(strings.Repeat("诊断 ", 100), 64)
	if len(got) > 64 || !strings.HasSuffix(got, "...[truncated]") || !utf8.ValidString(got) {
		t.Fatalf("SafeDiagnosticExcerpt() = %q (%d bytes)", got, len(got))
	}
}

func TestSafeDiagnosticExcerptRedactsCamelCaseJSONKeys(t *testing.T) {
	got := SafeDiagnosticExcerpt(
		`{"accessToken":"access-secret","nested":{"clientSecret":"client-secret"},"response":"useful reason"}`,
		1024,
	)
	for _, secret := range []string{"access-secret", "client-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("SafeDiagnosticExcerpt() leaked %q in %q", secret, got)
		}
	}
	if !strings.Contains(got, "useful reason") || !strings.Contains(got, redactedValue) {
		t.Fatalf("SafeDiagnosticExcerpt() lost safe response: %q", got)
	}
}

func TestSafeDiagnosticTextRedactsCamelCaseFieldsEmbeddedInText(t *testing.T) {
	got := SafeDiagnosticText(
		`request failed: {"clientToken":"access-secret","reason":"denied"}; ` +
			`retry clientSecret=client-secret; upstream unavailable`,
	)
	for _, secret := range []string{"access-secret", "client-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("SafeDiagnosticText() leaked %q in %q", secret, got)
		}
	}
	for _, diagnosis := range []string{"request failed", "denied", "upstream unavailable"} {
		if !strings.Contains(got, diagnosis) {
			t.Fatalf("SafeDiagnosticText() lost %q in %q", diagnosis, got)
		}
	}
}

func TestSafeDiagnosticTextRedactsJSONEmbeddedInJSONString(t *testing.T) {
	got := SafeDiagnosticText(
		`{"message":"request failed: {\"apiKey\":\"embedded-secret\"}","reason":"denied"}`,
	)
	if strings.Contains(got, "embedded-secret") {
		t.Fatalf("SafeDiagnosticText() leaked embedded JSON secret in %q", got)
	}
	if !strings.Contains(got, "request failed") || !strings.Contains(got, "denied") {
		t.Fatalf("SafeDiagnosticText() lost diagnosis: %q", got)
	}
}

func TestConsoleSanitizesLogValuerUnderErrorKey(t *testing.T) {
	var output strings.Builder
	logger := New(Config{Format: FormatConsole, Level: LevelDebug, Output: &output})
	logger.Error(context.Background(), "failed", Any("submit_error", secretErrorLogValuer{}))
	logs := output.String()
	if strings.Contains(logs, "raw-secret") || !strings.Contains(logs, "gateway timeout") {
		t.Fatalf("console log did not safely resolve error LogValuer: %s", logs)
	}
}

func TestLoggerBoundsResolvedStringErrorValues(t *testing.T) {
	for _, format := range []string{FormatJSON, FormatConsole} {
		t.Run(format, func(t *testing.T) {
			var output strings.Builder
			logger := New(Config{Format: format, Level: LevelDebug, Output: &output})
			logger.Error(
				context.Background(), "failed",
				Any("submit_error", largeErrorLogValuer{}),
			)
			logs := output.String()
			if !strings.Contains(logs, "...[truncated]") || strings.Count(logs, "x") >= maxSafeErrorBytes {
				t.Fatalf("logger did not bound resolved error value (%d bytes): %q", len(logs), logs)
			}
		})
	}
}

type largeErrorLogValuer struct{}

func (largeErrorLogValuer) LogValue() slog.Value {
	return slog.StringValue(strings.Repeat("x", maxSafeErrorBytes*2))
}

type secretErrorLogValuer struct{}

func (secretErrorLogValuer) LogValue() slog.Value {
	return slog.StringValue("token=raw-secret; gateway timeout")
}

func TestSafeErrorTextContainsPanickingError(t *testing.T) {
	if got := SafeErrorText(panickingError{}); got != unavailableErrorText {
		t.Fatalf("SafeErrorText(panickingError) = %q, want %q", got, unavailableErrorText)
	}
}

func TestSafeErrorTextIsBoundedAndRedactsSignedRequest(t *testing.T) {
	got := SafeErrorText(errors.New(
		`submit failed signedRequest="sensitive-payload"; reason=` + strings.Repeat("x", maxSafeErrorBytes),
	))
	if len(got) > maxSafeErrorBytes || !strings.HasSuffix(got, "...[truncated]") {
		t.Fatalf("SafeErrorText() length = %d, value = %q", len(got), got)
	}
	if strings.Contains(got, "sensitive-payload") || !strings.Contains(got, redactedValue) {
		t.Fatalf("SafeErrorText() did not redact signed request: %q", got)
	}
}

type panickingError struct{}

func (panickingError) Error() string { panic("password=do-not-log") }
