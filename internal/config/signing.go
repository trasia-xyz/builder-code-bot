package config

import (
	"fmt"
	"regexp"
	"strings"

	"builder-code-bot/internal/secret"
)

var addressPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

type SigningConfig struct {
	DecryptPassword secret.SecretString `toml:"decrypt_password"`
}

type BuilderConfig struct {
	Name                string              `toml:"name"`
	Address             string              `toml:"address"`
	EncryptedPrivateKey secret.SecretString `toml:"encrypted_private_key"`
}

type AccountConfig struct {
	Address             string              `toml:"address"`
	EncryptedPrivateKey secret.SecretString `toml:"encrypted_private_key"`
}

type PayoutConfig struct {
	RecipientAddress string `toml:"recipient_address"`
}

func (*SigningConfig) NormalizeAndValidate() error {
	return nil
}

func (cfg *Config) normalizeAndValidateAccounts() error {
	if len(cfg.Builders) == 0 {
		return fmt.Errorf("at least one builder is required")
	}

	names := make(map[string]struct{}, len(cfg.Builders))
	addresses := make(map[string]struct{}, len(cfg.Builders))
	for index := range cfg.Builders {
		builder := &cfg.Builders[index]
		builder.Name = strings.TrimSpace(builder.Name)
		builder.Address = strings.TrimSpace(builder.Address)
		if err := requireValue("builder name", builder.Name); err != nil {
			return fmt.Errorf("builders[%d]: %w", index, err)
		}
		if !addressPattern.MatchString(builder.Address) {
			return fmt.Errorf("builders[%d]: builder address must match %s", index, addressPattern)
		}
		if builder.EncryptedPrivateKey.Reveal() == "" {
			return fmt.Errorf("builders[%d]: encrypted_private_key is required", index)
		}
		if _, exists := names[builder.Name]; exists {
			return fmt.Errorf("duplicate builder name %q", builder.Name)
		}
		names[builder.Name] = struct{}{}
		canonicalAddress := strings.ToLower(builder.Address)
		if _, exists := addresses[canonicalAddress]; exists {
			return fmt.Errorf("duplicate builder address %q", builder.Address)
		}
		addresses[canonicalAddress] = struct{}{}
	}

	cfg.Settlement.Address = strings.TrimSpace(cfg.Settlement.Address)
	if !addressPattern.MatchString(cfg.Settlement.Address) {
		return fmt.Errorf("settlement address must match %s", addressPattern)
	}
	if cfg.Settlement.EncryptedPrivateKey.Reveal() == "" {
		return fmt.Errorf("settlement encrypted_private_key is required")
	}
	if _, exists := addresses[strings.ToLower(cfg.Settlement.Address)]; exists {
		return fmt.Errorf("settlement address must differ from builder addresses")
	}

	cfg.Payout.RecipientAddress = strings.TrimSpace(cfg.Payout.RecipientAddress)
	if !addressPattern.MatchString(cfg.Payout.RecipientAddress) {
		return fmt.Errorf("recipient address must match %s", addressPattern)
	}
	if strings.EqualFold(cfg.Payout.RecipientAddress, cfg.Settlement.Address) {
		return fmt.Errorf("recipient address must differ from settlement address")
	}
	return nil
}
