package config

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"builder-code-bot/internal/logging"
)

const (
	builderOneAddress = "0x1111111111111111111111111111111111111111"
	builderTwoAddress = "0x2222222222222222222222222222222222222222"
	settlementAddress = "0x3333333333333333333333333333333333333333"
	recipientAddress  = "0x4444444444444444444444444444444444444444"
)

func TestLoadFileRejectsUnknownField(t *testing.T) {
	path := writeConfig(t, "[mysql]\nhost = \"127.0.0.1\"\ntyop = 1\n")
	_, err := LoadFile(path)
	if err == nil || !strings.Contains(err.Error(), "tyop") {
		t.Fatalf("LoadFile() error = %v", err)
	}
}

func TestLoadFileDecodesMultipleBuildersAndSeparateSettlement(t *testing.T) {
	cfg, err := LoadFile(writeConfig(t, validConfig()))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if len(cfg.Builders) != 2 {
		t.Fatalf("len(Builders) = %d, want 2", len(cfg.Builders))
	}
	if cfg.Builders[0].Address != builderOneAddress || cfg.Builders[1].Address != builderTwoAddress {
		t.Fatalf("builder addresses = %q, %q", cfg.Builders[0].Address, cfg.Builders[1].Address)
	}
	if cfg.Settlement.Address != settlementAddress {
		t.Fatalf("Settlement.Address = %q", cfg.Settlement.Address)
	}
	if cfg.MySQL.Password.Reveal() != "" || cfg.Signing.DecryptPassword.Reveal() != "" {
		t.Fatal("empty passwords must be accepted and preserved")
	}
}

func TestLoadFileDecodesSecretStrings(t *testing.T) {
	content := strings.Replace(validConfig(), `password = ""`, `password = "database-secret"`, 1)
	content = strings.Replace(content, `decrypt_password = ""`, `decrypt_password = "signing-secret"`, 1)
	cfg, err := LoadFile(writeConfig(t, content))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MySQL.Password.Reveal() != "database-secret" || cfg.Signing.DecryptPassword.Reveal() != "signing-secret" {
		t.Fatal("secret strings were not decoded")
	}
}

func TestLoadFileAppliesLoggingDefaults(t *testing.T) {
	cfg, err := LoadFile(writeConfig(t, validConfig()))
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if cfg.Logging.Format != logging.FormatConsole || cfg.Logging.Level != logging.LevelInfo || cfg.Logging.Color != logging.ColorAuto {
		t.Fatalf("Logging = %+v", cfg.Logging)
	}
	if cfg.Logging.AddSource {
		t.Fatal("Logging.AddSource = true, want false")
	}
}

func TestLoadFileValidatesEnabledSES(t *testing.T) {
	content := strings.Replace(validConfig(), "[notification]\nenabled = false", "[notification]\nenabled = true", 1)
	_, err := LoadFile(writeConfig(t, content))
	if err == nil || !strings.Contains(err.Error(), "notification.ses.from") {
		t.Fatalf("LoadFile() error = %v", err)
	}
}

func TestNotificationNormalizeAndValidateDropsEmptyRecipients(t *testing.T) {
	cfg := NotificationConfig{SES: SESConfig{
		To:      []string{" first@example.com ", " ", "", "second@example.com"},
		ReplyTo: []string{" ", " reply@example.com "},
	}}

	if err := cfg.NormalizeAndValidate(); err != nil {
		t.Fatalf("NormalizeAndValidate() error = %v", err)
	}
	if got, want := strings.Join(cfg.SES.To, ","), "first@example.com,second@example.com"; got != want {
		t.Fatalf("SES.To = %q, want %q", got, want)
	}
	if got, want := strings.Join(cfg.SES.ReplyTo, ","), "reply@example.com"; got != want {
		t.Fatalf("SES.ReplyTo = %q, want %q", got, want)
	}
}

func TestNotificationNormalizeAndValidateRequiresNonEmptyRecipient(t *testing.T) {
	tests := []struct {
		name string
		to   []string
	}{
		{name: "empty list", to: []string{}},
		{name: "whitespace entry", to: []string{"   "}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NotificationConfig{
				Enabled: true,
				SES: SESConfig{
					From: "sender@example.com",
					To:   tt.to,
				},
			}
			if err := cfg.NormalizeAndValidate(); err == nil || !strings.Contains(err.Error(), "notification.ses.to") {
				t.Fatalf("NormalizeAndValidate() error = %v", err)
			}
		})
	}
}

func TestMySQLNormalizeAndValidateReportsMissingHostFirst(t *testing.T) {
	for range 100 {
		cfg := MySQLConfig{Port: 3306}
		err := cfg.NormalizeAndValidate()
		if err == nil || err.Error() != "host is required" {
			t.Fatalf("NormalizeAndValidate() error = %v, want %q", err, "host is required")
		}
	}
}

func TestLoadFileRejectsDuplicateBuilderName(t *testing.T) {
	content := strings.Replace(validConfig(), `name = "builder-2"`, `name = "builder-1"`, 1)
	_, err := LoadFile(writeConfig(t, content))
	if err == nil || !strings.Contains(err.Error(), "builder name") {
		t.Fatalf("LoadFile() error = %v", err)
	}
}

func TestLoadFileRejectsDuplicateBuilderAddress(t *testing.T) {
	content := strings.Replace(validConfig(), builderTwoAddress, builderOneAddress, 1)
	_, err := LoadFile(writeConfig(t, content))
	if err == nil || !strings.Contains(err.Error(), "builder address") {
		t.Fatalf("LoadFile() error = %v", err)
	}
}

func TestLoadFileRejectsSettlementBuilderAddress(t *testing.T) {
	content := strings.Replace(validConfig(), settlementAddress, builderOneAddress, 1)
	_, err := LoadFile(writeConfig(t, content))
	if err == nil || !strings.Contains(err.Error(), "settlement") {
		t.Fatalf("LoadFile() error = %v", err)
	}
}

func TestLoadFileRejectsRecipientSettlementAddress(t *testing.T) {
	content := strings.Replace(validConfig(), recipientAddress, settlementAddress, 1)
	_, err := LoadFile(writeConfig(t, content))
	if err == nil || !strings.Contains(err.Error(), "recipient") {
		t.Fatalf("LoadFile() error = %v", err)
	}
}

func TestLoadFileRequiresBuilder(t *testing.T) {
	content := validConfig()
	start := strings.Index(content, "[[builders]]")
	end := strings.Index(content, "[settlement]")
	content = content[:start] + content[end:]
	_, err := LoadFile(writeConfig(t, content))
	if err == nil || !strings.Contains(err.Error(), "builder") {
		t.Fatalf("LoadFile() error = %v", err)
	}
}

func TestResolveBaseURLUsesOverride(t *testing.T) {
	got, err := ResolveBaseURL(HyperliquidConfig{Network: "testnet", BaseURL: "http://127.0.0.1:8080"})
	if err != nil || got != "http://127.0.0.1:8080" {
		t.Fatalf("ResolveBaseURL() = %q, %v", got, err)
	}
}

func TestResolveBaseURLUsesNetworkDefault(t *testing.T) {
	tests := []struct {
		network string
		want    string
	}{
		{network: "mainnet", want: MainnetBaseURL},
		{network: "testnet", want: TestnetBaseURL},
	}
	for _, tt := range tests {
		t.Run(tt.network, func(t *testing.T) {
			got, err := ResolveBaseURL(HyperliquidConfig{Network: tt.network})
			if err != nil || got != tt.want {
				t.Fatalf("ResolveBaseURL() = %q, %v", got, err)
			}
		})
	}
}

func TestResolveBaseURLRejectsUnknownNetwork(t *testing.T) {
	_, err := ResolveBaseURL(HyperliquidConfig{Network: "devnet"})
	if err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("ResolveBaseURL() error = %v", err)
	}
}

func TestValidateFileModeRejectsGroupOrOtherPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not available")
	}
	path := writeConfig(t, validConfig())
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod config: %v", err)
	}
	if err := ValidateFileMode(path); err == nil {
		t.Fatal("ValidateFileMode() error = nil")
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/config.toml"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod config: %v", err)
	}
	return path
}

func validConfig() string {
	return `[mysql]
host = "127.0.0.1"
port = 3306
database = "builder_code"
user = "service"
password = ""

[hyperliquid]
network = "mainnet"

[signing]
decrypt_password = ""

[[builders]]
name = "builder-1"
address = "` + builderOneAddress + `"
encrypted_private_key = "ciphertext-one"

[[builders]]
name = "builder-2"
address = "` + builderTwoAddress + `"
encrypted_private_key = "ciphertext-two"

[settlement]
address = "` + settlementAddress + `"
encrypted_private_key = "settlement-ciphertext"

[payout]
recipient_address = "` + recipientAddress + `"

[aws]
region = "ap-northeast-1"
profile = ""

[notification]
enabled = false

[notification.ses]
from = ""
to = []
reply_to = []
subject_prefix = "[builder-code]"
`
}
