package config

import (
	"fmt"
	"strings"

	"hyperliquid-builder-code-bot/internal/logging"
)

type LoggingConfig struct {
	Format    string `toml:"format"`
	Level     string `toml:"level"`
	Color     string `toml:"color"`
	AddSource bool   `toml:"add_source"`
}

func (cfg *LoggingConfig) NormalizeAndValidate() error {
	cfg.Format = strings.ToLower(strings.TrimSpace(cfg.Format))
	cfg.Level = strings.ToLower(strings.TrimSpace(cfg.Level))
	cfg.Color = strings.ToLower(strings.TrimSpace(cfg.Color))
	if err := logging.ValidateFormat(cfg.Format); err != nil {
		return fmt.Errorf("format: %w", err)
	}
	if err := logging.ValidateLevel(cfg.Level); err != nil {
		return fmt.Errorf("level: %w", err)
	}
	if err := logging.ValidateColor(cfg.Color); err != nil {
		return fmt.Errorf("color: %w", err)
	}
	return nil
}
