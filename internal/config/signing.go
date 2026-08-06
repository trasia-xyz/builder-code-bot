package config

import (
	"fmt"
	"regexp"
	"strings"

	"builder-code-bot/internal/secret"
)

const zeroAddress = "0x0000000000000000000000000000000000000000"

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

	if err := normalizeAndValidatePayoutRecipient(
		"referral payout",
		&cfg.ReferralPayout,
		cfg.Settlement.Address,
		addresses,
	); err != nil {
		return err
	}
	if err := normalizeAndValidatePayoutRecipient(
		"points rebate payout",
		&cfg.PointsRebatePayout,
		cfg.Settlement.Address,
		addresses,
	); err != nil {
		return err
	}
	if strings.EqualFold(
		cfg.ReferralPayout.RecipientAddress,
		cfg.PointsRebatePayout.RecipientAddress,
	) {
		return fmt.Errorf("referral and points rebate payout recipient addresses must differ")
	}
	return nil
}

func normalizeAndValidatePayoutRecipient(
	name string,
	payout *PayoutConfig,
	settlementAddress string,
	builderAddresses map[string]struct{},
) error {
	payout.RecipientAddress = strings.TrimSpace(payout.RecipientAddress)
	if !addressPattern.MatchString(payout.RecipientAddress) {
		return fmt.Errorf("%s recipient address must match %s", name, addressPattern)
	}
	recipientAddress := strings.ToLower(payout.RecipientAddress)
	if recipientAddress == zeroAddress {
		return fmt.Errorf("%s recipient address must not be the zero address", name)
	}
	if strings.EqualFold(payout.RecipientAddress, settlementAddress) {
		return fmt.Errorf("%s recipient address must differ from settlement address", name)
	}
	if _, exists := builderAddresses[recipientAddress]; exists {
		return fmt.Errorf("%s recipient address must differ from builder addresses", name)
	}
	return nil
}
