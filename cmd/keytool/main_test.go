package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"builder-code-bot/internal/crypt/keycipher"
	"builder-code-bot/internal/secret"
)

const testPrivateKeyHex = "0x0000000000000000000000000000000000000000000000000000000000000001"

func TestEncryptProducesDecryptableConfigValue(t *testing.T) {
	prompt := &fakePrompt{
		secrets: []secret.SecretString{
			secret.NewString(testPrivateKeyHex),
			secret.NewString("passphrase"),
			secret.NewString("passphrase"),
		},
	}
	var stdout bytes.Buffer

	err := run(context.Background(), []string{"encrypt"}, &stdout, nil, prompt)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "signer_address = 0x") {
		t.Fatalf("encrypt output missing signer address: %s", out)
	}
	encrypted := parseQuotedValue(t, out, "encrypted_private_key = ")
	if _, err := base64.RawStdEncoding.DecodeString(encrypted); err != nil {
		t.Fatalf("encrypted key is not raw base64: %v", err)
	}
	if strings.Contains(encrypted, ":") {
		t.Fatalf("encrypted key contains prefix separator: %q", encrypted)
	}
	if strings.Contains(encrypted, testPrivateKeyHex) {
		t.Fatal("encrypted key leaked plaintext")
	}

	privateKey, err := keycipher.Decrypt(secret.NewString(encrypted), secret.NewString("passphrase"))
	if err != nil {
		t.Fatalf("decrypt encrypted output: %v", err)
	}
	if privateKey.Reveal() != testPrivateKeyHex {
		t.Fatalf("decrypted private key = %q", privateKey.Reveal())
	}
}

func TestDecryptHidesPrivateKeyByDefault(t *testing.T) {
	encrypted := encryptedTestPrivateKey(t)
	prompt := &fakePrompt{
		lines:   []string{encrypted.Reveal()},
		secrets: []secret.SecretString{secret.NewString("passphrase")},
	}
	var stdout bytes.Buffer

	err := run(context.Background(), []string{"decrypt"}, &stdout, nil, prompt)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "signer_address = 0x") {
		t.Fatalf("decrypt output missing signer address: %s", out)
	}
	if strings.Contains(out, testPrivateKeyHex) || strings.Contains(out, "private_key") {
		t.Fatalf("decrypt output leaked private key by default: %s", out)
	}
}

func TestDecryptShowsPrivateKeyWhenRequested(t *testing.T) {
	encrypted := encryptedTestPrivateKey(t)
	prompt := &fakePrompt{
		lines:   []string{encrypted.Reveal()},
		secrets: []secret.SecretString{secret.NewString("passphrase")},
	}
	var stdout bytes.Buffer

	err := run(context.Background(), []string{"decrypt", "-show-private-key"}, &stdout, nil, prompt)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, `private_key = "`+testPrivateKeyHex+`"`) {
		t.Fatalf("decrypt output missing private key: %s", out)
	}
}

func TestDecryptAcceptsConfigAssignmentLine(t *testing.T) {
	encrypted := encryptedTestPrivateKey(t)
	prompt := &fakePrompt{
		lines:   []string{`encrypted_private_key = "` + encrypted.Reveal() + `"`},
		secrets: []secret.SecretString{secret.NewString("passphrase")},
	}
	var stdout bytes.Buffer

	err := run(context.Background(), []string{"decrypt"}, &stdout, nil, prompt)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !strings.Contains(stdout.String(), "signer_address = 0x") {
		t.Fatalf("decrypt output missing signer address: %s", stdout.String())
	}
}

func TestEncryptRejectsPasswordMismatch(t *testing.T) {
	prompt := &fakePrompt{
		secrets: []secret.SecretString{
			secret.NewString(testPrivateKeyHex),
			secret.NewString("passphrase"),
			secret.NewString("different"),
		},
	}

	err := run(context.Background(), []string{"encrypt"}, nil, nil, prompt)
	if err == nil {
		t.Fatal("expected password mismatch error")
	}
	if !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("unexpected error: %v", err)
	}
}

type fakePrompt struct {
	lines   []string
	secrets []secret.SecretString
}

func (p *fakePrompt) ReadLine(label string) (string, error) {
	if len(p.lines) == 0 {
		return "", nil
	}
	value := p.lines[0]
	p.lines = p.lines[1:]
	return value, nil
}

func (p *fakePrompt) ReadSecret(label string) (secret.SecretString, error) {
	if len(p.secrets) == 0 {
		return secret.SecretString{}, nil
	}
	value := p.secrets[0]
	p.secrets = p.secrets[1:]
	return value, nil
}

func encryptedTestPrivateKey(t *testing.T) secret.SecretString {
	t.Helper()
	encrypted, err := keycipher.Encrypt(secret.NewString(testPrivateKeyHex), secret.NewString("passphrase"))
	if err != nil {
		t.Fatalf("encrypt private key: %v", err)
	}
	return encrypted
}

func parseQuotedValue(t *testing.T, output string, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		return strings.Trim(strings.TrimPrefix(line, prefix), `"`)
	}
	t.Fatalf("missing %q in output: %s", prefix, output)
	return ""
}
