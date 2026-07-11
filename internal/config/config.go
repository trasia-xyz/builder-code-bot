package config

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	MySQL        MySQLConfig        `mapstructure:"mysql"`
	Hyperliquid  HyperliquidConfig  `mapstructure:"hyperliquid"`
	Signing      SigningConfig      `mapstructure:"signing"`
	Builders     []BuilderConfig    `mapstructure:"builders"`
	Settlement   AccountConfig      `mapstructure:"settlement"`
	Payout       PayoutConfig       `mapstructure:"payout"`
	Logging      LoggingConfig      `mapstructure:"logging"`
	AWS          AWSConfig          `mapstructure:"aws"`
	Notification NotificationConfig `mapstructure:"notification"`
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

	cfg := Default()
	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return Config{}, err
	}
	if err := v.UnmarshalExact(&cfg, viper.DecodeHook(decodeHook())); err != nil {
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
