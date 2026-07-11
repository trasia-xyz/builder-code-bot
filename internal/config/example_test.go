package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestExampleConfigLoads(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test path")
	}
	source := filepath.Join(filepath.Dir(filename), "..", "..", "config.example.toml")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Builders) != 2 || cfg.Notification.SES.SubjectPrefix != "[builder-code]" {
		t.Fatalf("example config = %#v", cfg)
	}
}
