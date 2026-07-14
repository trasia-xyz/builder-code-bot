package config

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	MainnetBaseURL = "https://api.hyperliquid.xyz"
	TestnetBaseURL = "https://api.hyperliquid-testnet.xyz"
)

type HyperliquidConfig struct {
	Network string `toml:"network"`
	BaseURL string `toml:"base_url"`
}

func (cfg *HyperliquidConfig) NormalizeAndValidate() error {
	cfg.Network = strings.ToLower(strings.TrimSpace(cfg.Network))
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	_, err := ResolveBaseURL(*cfg)
	return err
}

func ResolveBaseURL(cfg HyperliquidConfig) (string, error) {
	var defaultURL string
	switch strings.ToLower(strings.TrimSpace(cfg.Network)) {
	case "mainnet":
		defaultURL = MainnetBaseURL
	case "testnet":
		defaultURL = TestnetBaseURL
	default:
		return "", fmt.Errorf("network must be %q or %q", "mainnet", "testnet")
	}

	override := strings.TrimSpace(cfg.BaseURL)
	if override == "" {
		return defaultURL, nil
	}
	parsed, err := url.ParseRequestURI(override)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("base_url must be an absolute HTTP or HTTPS URL")
	}
	return override, nil
}
