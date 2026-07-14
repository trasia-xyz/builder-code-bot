package config

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	MySQL        MySQLConfig        `toml:"mysql"`
	Hyperliquid  HyperliquidConfig  `toml:"hyperliquid"`
	Signing      SigningConfig      `toml:"signing"`
	Builders     []BuilderConfig    `toml:"builders"`
	Settlement   AccountConfig      `toml:"settlement"`
	Payout       PayoutConfig       `toml:"payout"`
	Logging      LoggingConfig      `toml:"logging"`
	AWS          AWSConfig          `toml:"aws"`
	Notification NotificationConfig `toml:"notification"`
}

func Default() Config {
	return Config{
		MySQL:       MySQLConfig{Port: 3306},
		Hyperliquid: HyperliquidConfig{Network: "mainnet"},
		Logging: LoggingConfig{
			Format: "console",
			Level:  "info",
			Color:  "auto",
		},
		Notification: NotificationConfig{
			SES: SESConfig{SubjectPrefix: "[builder-code]"},
		},
	}
}

func LoadFile(path string) (Config, error) {
	if err := ValidateFileMode(path); err != nil {
		return Config{}, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := Default()
	decoder := toml.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			return Config{}, fmt.Errorf("decode config %s: %s", path, strict.String())
		}
		return Config{}, fmt.Errorf("decode config %s: %w", path, err)
	}
	if err := cfg.NormalizeAndValidate(); err != nil {
		return Config{}, fmt.Errorf("validate config %s: %w", path, err)
	}
	return cfg, nil
}

func (cfg *Config) NormalizeAndValidate() error {
	validators := []struct {
		name string
		fn   func() error
	}{
		{name: "mysql", fn: cfg.MySQL.NormalizeAndValidate},
		{name: "hyperliquid", fn: cfg.Hyperliquid.NormalizeAndValidate},
		{name: "signing", fn: cfg.Signing.NormalizeAndValidate},
		{name: "accounts", fn: cfg.normalizeAndValidateAccounts},
		{name: "logging", fn: cfg.Logging.NormalizeAndValidate},
		{name: "aws", fn: cfg.AWS.NormalizeAndValidate},
		{name: "notification", fn: cfg.Notification.NormalizeAndValidate},
	}
	for _, validator := range validators {
		if err := validator.fn(); err != nil {
			return fmt.Errorf("%s: %w", validator.name, err)
		}
	}
	if cfg.Notification.Enabled && cfg.AWS.Region == "" {
		return fmt.Errorf("aws.region is required when notification is enabled")
	}
	return nil
}

func ValidateFileMode(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat config %s: %w", path, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("config %s permissions %04o allow group or other access; require 0600 or stricter", path, info.Mode().Perm())
	}
	return nil
}

func requireValue(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}
