package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestRunHelpPrintsAvailableFlagsAndSucceeds(t *testing.T) {
	var output bytes.Buffer
	if err := runWithOutput([]string{"--help"}, &output); err != nil {
		t.Fatalf("runWithOutput(--help) error = %v, want nil", err)
	}
	got := output.String()
	for _, want := range []string{
		"Usage: builder-code-bot [flags]",
		"Available flags:",
		"-config",
		"-consolidate-only",
		"-run-on-start",
		"-test-ses",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("--help output missing %q:\n%s", want, got)
		}
	}
}

func TestRunHelpReturnsOutputError(t *testing.T) {
	writeErr := errors.New("terminal output unavailable")
	err := runWithOutput([]string{"--help"}, errorWriter{err: writeErr})
	if !errors.Is(err, writeErr) {
		t.Fatalf("runWithOutput(--help) error = %v, want output error", err)
	}
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestParseOptionsDefaults(t *testing.T) {
	opts, err := parseOptions(nil)
	if err != nil {
		t.Fatalf("parseOptions() error = %v", err)
	}
	if opts.ConfigPath != "./config.toml" {
		t.Fatalf("config path = %q, want ./config.toml", opts.ConfigPath)
	}
	if opts.RunOnStart {
		t.Fatal("run-on-start default = true, want false")
	}
	if opts.TestSES {
		t.Fatal("test-ses default = true, want false")
	}
	if opts.ConsolidateOnly {
		t.Fatal("consolidate-only default = true, want false")
	}
}

func TestParseOptionsUsesTestSESBooleanValue(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want bool
	}{
		{args: []string{"-test-ses"}, want: true},
		{args: []string{"-test-ses=false"}, want: false},
	} {
		opts, err := parseOptions(tt.args)
		if err != nil {
			t.Fatalf("parseOptions(%v) error = %v", tt.args, err)
		}
		if opts.TestSES != tt.want {
			t.Fatalf("parseOptions(%v) TestSES = %v, want %v", tt.args, opts.TestSES, tt.want)
		}
	}
}

func TestParseOptionsRejectsTestSESWithRunOnStart(t *testing.T) {
	if _, err := parseOptions([]string{"-test-ses", "-run-on-start"}); err == nil {
		t.Fatal("parseOptions() error = nil, want mutually exclusive flag error")
	}
}

func TestParseOptionsUsesConsolidateOnlyBooleanValue(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want bool
	}{
		{args: []string{"-consolidate-only"}, want: true},
		{args: []string{"-consolidate-only=false"}, want: false},
	} {
		opts, err := parseOptions(tt.args)
		if err != nil {
			t.Fatalf("parseOptions(%v) error = %v", tt.args, err)
		}
		if opts.ConsolidateOnly != tt.want {
			t.Fatalf(
				"parseOptions(%v) ConsolidateOnly = %v, want %v",
				tt.args, opts.ConsolidateOnly, tt.want,
			)
		}
	}
}

func TestParseOptionsRejectsConsolidateOnlyWithOtherModes(t *testing.T) {
	for _, args := range [][]string{
		{"-consolidate-only", "-run-on-start"},
		{"-consolidate-only", "-test-ses"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("parseOptions(%v) error = nil, want mutually exclusive flag error", args)
		}
	}
}

func TestParseOptionsUsesRunOnStartBooleanValue(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want bool
	}{
		{args: []string{"-run-on-start"}, want: true},
		{args: []string{"-run-on-start=false"}, want: false},
	} {
		opts, err := parseOptions(tt.args)
		if err != nil {
			t.Fatalf("parseOptions(%v) error = %v", tt.args, err)
		}
		if opts.RunOnStart != tt.want {
			t.Fatalf("parseOptions(%v) RunOnStart = %v, want %v", tt.args, opts.RunOnStart, tt.want)
		}
	}
}

func TestParseOptionsAcceptsConfigPath(t *testing.T) {
	opts, err := parseOptions([]string{"-config", "/secure/service.toml"})
	if err != nil {
		t.Fatalf("parseOptions() error = %v", err)
	}
	if opts.ConfigPath != "/secure/service.toml" {
		t.Fatalf("config path = %q", opts.ConfigPath)
	}
}

func TestParseOptionsRejectsPositionalArguments(t *testing.T) {
	if _, err := parseOptions([]string{"unexpected"}); err == nil {
		t.Fatal("parseOptions() error = nil, want positional argument error")
	}
}
