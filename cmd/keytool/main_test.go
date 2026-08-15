package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
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

func TestCommandOutputErrorsAreReturned(t *testing.T) {
	writeErr := errors.New("terminal output unavailable")
	tests := []struct {
		name   string
		args   []string
		prompt *fakePrompt
		failAt int
	}{
		{
			name: "encrypt signer", args: []string{"encrypt"}, failAt: 1,
			prompt: encryptPrompt(),
		},
		{
			name: "encrypt ciphertext", args: []string{"encrypt"}, failAt: 2,
			prompt: encryptPrompt(),
		},
		{
			name: "decrypt signer", args: []string{"decrypt"}, failAt: 1,
			prompt: decryptPrompt(t),
		},
		{
			name: "decrypt private key", args: []string{"decrypt", "-show-private-key"}, failAt: 2,
			prompt: decryptPrompt(t),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := &failNthWriter{failAt: tt.failAt, err: writeErr}
			err := run(context.Background(), tt.args, writer, io.Discard, tt.prompt)
			if !errors.Is(err, writeErr) {
				t.Fatalf("run() error = %v, want output error", err)
			}
		})
	}
}

func TestUsageAndPromptOutputErrorsAreReturned(t *testing.T) {
	writeErr := errors.New("terminal output unavailable")
	if err := run(
		context.Background(), []string{"help"},
		&failNthWriter{failAt: 1, err: writeErr}, io.Discard, &fakePrompt{},
	); !errors.Is(err, writeErr) {
		t.Fatalf("help error = %v, want output error", err)
	}
	prompt := &terminalPrompt{
		stderr: &failNthWriter{failAt: 1, err: writeErr},
		reader: bufio.NewReader(strings.NewReader("value\n")),
	}
	if _, err := prompt.ReadLine("label: "); !errors.Is(err, writeErr) {
		t.Fatalf("ReadLine() error = %v, want prompt output error", err)
	}
}

func encryptPrompt() *fakePrompt {
	return &fakePrompt{secrets: []secret.SecretString{
		secret.NewString(testPrivateKeyHex),
		secret.NewString("passphrase"),
		secret.NewString("passphrase"),
	}}
}

func decryptPrompt(t *testing.T) *fakePrompt {
	t.Helper()
	return &fakePrompt{
		lines:   []string{encryptedTestPrivateKey(t).Reveal()},
		secrets: []secret.SecretString{secret.NewString("passphrase")},
	}
}

type failNthWriter struct {
	writes int
	failAt int
	err    error
}

func (w *failNthWriter) Write(data []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, w.err
	}
	return len(data), nil
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
