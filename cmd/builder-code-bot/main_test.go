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

func TestParseOptionsUsesRunOnStartFlagPresence(t *testing.T) {
	for _, args := range [][]string{{"-run-on-start"}, {"-run-on-start=false"}} {
		opts, err := parseOptions(args)
		if err != nil {
			t.Fatalf("parseOptions(%v) error = %v", args, err)
		}
		if !opts.RunOnStart {
			t.Fatalf("parseOptions(%v) RunOnStart = false, want true for present flag", args)
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
