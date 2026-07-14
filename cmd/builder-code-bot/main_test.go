package main

import "testing"

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
