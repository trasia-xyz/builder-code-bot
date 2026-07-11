package logging

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestValidateOptions(t *testing.T) {
	tests := []struct {
		name    string
		valid   func(string) error
		value   string
		wantErr string
	}{
		{name: "format console", valid: ValidateFormat, value: "console"},
		{name: "format json", valid: ValidateFormat, value: "JSON"},
		{name: "format invalid", valid: ValidateFormat, value: "plain", wantErr: "console"},
		{name: "level debug", valid: ValidateLevel, value: "debug"},
		{name: "level error", valid: ValidateLevel, value: " ERROR "},
		{name: "level invalid", valid: ValidateLevel, value: "trace", wantErr: "debug"},
		{name: "color auto", valid: ValidateColor, value: "auto"},
		{name: "color never", valid: ValidateColor, value: "NEVER"},
		{name: "color invalid", valid: ValidateColor, value: "always", wantErr: "auto"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.valid(tt.value)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected valid option, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		value string
		want  slog.Level
	}{
		{value: "", want: slog.LevelInfo},
		{value: "debug", want: slog.LevelDebug},
		{value: "INFO", want: slog.LevelInfo},
		{value: " warn ", want: slog.LevelWarn},
		{value: "error", want: slog.LevelError},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := ParseLevel(tt.value)
			if err != nil {
				t.Fatalf("parse level: %v", err)
			}
			if got != tt.want {
				t.Fatalf("unexpected level: got %v want %v", got, tt.want)
			}
		})
	}
}

func TestParseLevelRejectsUnknownLevel(t *testing.T) {
	_, err := ParseLevel("trace")
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "unknown log level") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMustParseLevel(t *testing.T) {
	if got := MustParseLevel("warn"); got != slog.LevelWarn {
		t.Fatalf("unexpected level: got %v want %v", got, slog.LevelWarn)
	}
}

func TestMustParseLevelPanicsForUnknownLevel(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()

	_ = MustParseLevel("trace")
}

func TestLevelNameFallback(t *testing.T) {
	got := levelName(slog.Level(12))
	if got != "ERROR+4" {
		t.Fatalf("unexpected level name: %q", got)
	}
}

func TestNormalizeFormat(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{value: "", want: FormatJSON},
		{value: " CONSOLE ", want: FormatConsole},
		{value: "JSON", want: FormatJSON},
		{value: "plain", want: FormatConsole},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			if got := normalizeFormat(tt.value); got != tt.want {
				t.Fatalf("unexpected format: got %q want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeColor(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{value: "", want: ColorAuto},
		{value: " AUTO ", want: ColorAuto},
		{value: "NEVER", want: ColorNever},
		{value: "always", want: ColorAuto},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			if got := normalizeColor(tt.value); got != tt.want {
				t.Fatalf("unexpected color: got %q want %q", got, tt.want)
			}
		})
	}
}

func TestShouldColorFalseBranches(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer file.Close()

	tests := []struct {
		name   string
		format string
		color  string
		output any
	}{
		{name: "json format", format: FormatJSON, color: ColorAuto, output: os.Stdout},
		{name: "never color", format: FormatConsole, color: ColorNever, output: os.Stdout},
		{name: "non file output", format: FormatConsole, color: ColorAuto, output: new(bytes.Buffer)},
		{name: "regular file output", format: FormatConsole, color: ColorAuto, output: file},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer, ok := tt.output.(interface {
				Write([]byte) (int, error)
			})
			if !ok {
				t.Fatalf("test output does not implement writer: %#v", tt.output)
			}
			if shouldColor(tt.format, tt.color, writer) {
				t.Fatalf("expected color to be disabled")
			}
		})
	}
}
