package config

import (
	"fmt"
	"strings"

	"hyperliquid-builder-code-bot/internal/secret"
)

type MySQLConfig struct {
	Host     string              `mapstructure:"host"`
	Port     int                 `mapstructure:"port"`
	Database string              `mapstructure:"database"`
	User     string              `mapstructure:"user"`
	Password secret.SecretString `mapstructure:"password"`
}

func (cfg *MySQLConfig) NormalizeAndValidate() error {
	cfg.Host = strings.TrimSpace(cfg.Host)
	cfg.Database = strings.TrimSpace(cfg.Database)
	cfg.User = strings.TrimSpace(cfg.User)
	if err := requireValue("host", cfg.Host); err != nil {
		return err
	}
	if err := requireValue("database", cfg.Database); err != nil {
		return err
	}
	if err := requireValue("user", cfg.User); err != nil {
		return err
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return nil
}
