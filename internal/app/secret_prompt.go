package app

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"

	"builder-code-bot/internal/config"
	"builder-code-bot/internal/crypt/keycipher"
	"builder-code-bot/internal/hyperliquid/signing"
	"builder-code-bot/internal/secret"
)

type SecretPrompt interface {
	ReadSecret(label string) (secret.SecretString, error)
}

type terminalPrompt struct {
	input  *os.File
	output io.Writer
}

func newTerminalPrompt(input *os.File, output io.Writer) *terminalPrompt {
	if output == nil {
		output = io.Discard
	}
	return &terminalPrompt{input: input, output: output}
}

func newTerminalPromptFile() *terminalPrompt {
	return newTerminalPrompt(os.Stdin, os.Stderr)
}

func (p *terminalPrompt) ReadSecret(label string) (secret.SecretString, error) {
	if p == nil || p.input == nil || !term.IsTerminal(uintptr(p.input.Fd())) {
		return secret.SecretString{}, fmt.Errorf("secret input must be entered from a terminal")
	}
	if _, err := fmt.Fprint(p.output, label); err != nil {
		return secret.SecretString{}, fmt.Errorf("write secret prompt: %w", err)
	}
	raw, err := term.ReadPassword(uintptr(p.input.Fd()))
	_, _ = fmt.Fprintln(p.output)
	if err != nil {
		return secret.SecretString{}, fmt.Errorf("read secret from terminal: %w", err)
	}
	defer zeroBytes(raw)
	return secret.NewString(string(raw)), nil
}

func resolvePassword(configured secret.SecretString, prompt SecretPrompt) (secret.SecretString, error) {
	if configured.Reveal() != "" {
		return configured, nil
	}
	if prompt == nil {
		return secret.SecretString{}, fmt.Errorf("secret prompt is required")
	}
	password, err := prompt.ReadSecret("Enter private key decryption password: ")
	if err != nil {
		return secret.SecretString{}, fmt.Errorf("read private key decryption password: %w", err)
	}
	if password.Reveal() == "" {
		return secret.SecretString{}, fmt.Errorf("private key decryption password is empty")
	}
	return password, nil
}

func buildSigners(cfg config.Config, password secret.SecretString) (map[string]signing.PrivateKey, error) {
	signers := make(map[string]signing.PrivateKey, len(cfg.Builders)+1)
	for _, builder := range cfg.Builders {
		if err := addSigner(signers, builder.Name, builder.Address, builder.EncryptedPrivateKey, password); err != nil {
			return nil, err
		}
	}
	if err := addSigner(signers, "settlement", cfg.Settlement.Address, cfg.Settlement.EncryptedPrivateKey, password); err != nil {
		return nil, err
	}
	return signers, nil
}

func addSigner(signers map[string]signing.PrivateKey, name, configuredAddress string, encrypted, password secret.SecretString) error {
	plaintext, err := keycipher.Decrypt(encrypted, password)
	if err != nil {
		return fmt.Errorf("decrypt signer %q: %w", name, err)
	}
	key, err := signing.ParsePrivateKey(plaintext)
	if err != nil {
		return fmt.Errorf("parse signer %q: %w", name, err)
	}
	derivedAddress, err := key.Address()
	if err != nil {
		return fmt.Errorf("derive signer %q address: %w", name, err)
	}
	if !strings.EqualFold(configuredAddress, derivedAddress) {
		return fmt.Errorf("signer %q configured address does not match derived address", name)
	}
	signers[configuredAddress] = key
	return nil
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
