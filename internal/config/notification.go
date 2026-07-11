package config

import (
	"fmt"
	"strings"
)

type AWSConfig struct {
	Region  string `mapstructure:"region"`
	Profile string `mapstructure:"profile"`
}

type NotificationConfig struct {
	Enabled bool      `mapstructure:"enabled"`
	SES     SESConfig `mapstructure:"ses"`
}

type SESConfig struct {
	From          string   `mapstructure:"from"`
	To            []string `mapstructure:"to"`
	ReplyTo       []string `mapstructure:"reply_to"`
	SubjectPrefix string   `mapstructure:"subject_prefix"`
}

func (cfg *AWSConfig) NormalizeAndValidate() error {
	cfg.Region = strings.TrimSpace(cfg.Region)
	cfg.Profile = strings.TrimSpace(cfg.Profile)
	return nil
}

func (cfg *NotificationConfig) NormalizeAndValidate() error {
	cfg.SES.From = strings.TrimSpace(cfg.SES.From)
	cfg.SES.SubjectPrefix = strings.TrimSpace(cfg.SES.SubjectPrefix)
	cfg.SES.To = normalizeStrings(cfg.SES.To)
	cfg.SES.ReplyTo = normalizeStrings(cfg.SES.ReplyTo)
	if !cfg.Enabled {
		return nil
	}
	if cfg.SES.From == "" {
		return fmt.Errorf("notification.ses.from is required when notification is enabled")
	}
	if len(cfg.SES.To) == 0 {
		return fmt.Errorf("notification.ses.to is required when notification is enabled")
	}
	return nil
}

func normalizeStrings(values []string) []string {
	normalized := values[:0]
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			normalized = append(normalized, value)
		}
	}
	return normalized
}
