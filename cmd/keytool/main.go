package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/term"

	"builder-code-bot/internal/crypt/keycipher"
	"builder-code-bot/internal/hyperliquid/signing"
	"builder-code-bot/internal/secret"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, newTerminalPrompt(os.Stdin, os.Stderr)); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "keytool: %v\n", err)
		}
		os.Exit(1)
	}
}

type prompt interface {
	ReadLine(label string) (string, error)
	ReadSecret(label string) (secret.SecretString, error)
}

type terminalPrompt struct {
	stdin  *os.File
	stderr io.Writer
	reader *bufio.Reader
}

func newTerminalPrompt(stdin *os.File, stderr io.Writer) *terminalPrompt {
	if stderr == nil {
		stderr = io.Discard
	}
	return &terminalPrompt{
		stdin:  stdin,
		stderr: stderr,
		reader: bufio.NewReader(stdin),
	}
}

func (p *terminalPrompt) ReadLine(label string) (string, error) {
	fmt.Fprint(p.stderr, label)
	value, err := p.reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func (p *terminalPrompt) ReadSecret(label string) (secret.SecretString, error) {
	fd := uintptr(p.stdin.Fd())
	if !term.IsTerminal(fd) {
		return secret.SecretString{}, fmt.Errorf("secret input must be entered from a terminal")
	}
	fmt.Fprint(p.stderr, label)
	raw, err := term.ReadPassword(fd)
	fmt.Fprintln(p.stderr)
	if err != nil {
		return secret.SecretString{}, err
	}
	defer zeroBytes(raw)
	return secret.NewString(string(raw)), nil
}

func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, prompt prompt) error {
	if ctx == nil {
		return fmt.Errorf("context is nil")
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if prompt == nil {
		return fmt.Errorf("prompt is nil")
	}
	if len(args) == 0 {
		printUsage(stderr)
		return flag.ErrHelp
	}

	switch args[0] {
	case "encrypt":
		return runEncrypt(args[1:], stdout, stderr, prompt)
	case "decrypt":
		return runDecrypt(args[1:], stdout, stderr, prompt)
	case "-h", "--help", "help":
		printUsage(stdout)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runEncrypt(args []string, stdout io.Writer, stderr io.Writer, prompt prompt) error {
	fs := flag.NewFlagSet("keytool encrypt", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("encrypt does not accept positional arguments")
	}

	privateKey, err := prompt.ReadSecret("Enter private key: ")
	if err != nil {
		return fmt.Errorf("read private key: %w", err)
	}
	password, err := prompt.ReadSecret("Enter encryption password: ")
	if err != nil {
		return fmt.Errorf("read encryption password: %w", err)
	}
	confirm, err := prompt.ReadSecret("Confirm encryption password: ")
	if err != nil {
		return fmt.Errorf("confirm encryption password: %w", err)
	}
	if password.Reveal() != confirm.Reveal() {
		return fmt.Errorf("encryption passwords do not match")
	}

	key, err := signing.ParsePrivateKey(privateKey)
	if err != nil {
		return err
	}
	address, err := key.Address()
	if err != nil {
		return err
	}
	encrypted, err := keycipher.Encrypt(privateKey, password)
	if err != nil {
		return err
	}

	fmt.Fprintln(stdout, "signer_address =", address)
	fmt.Fprintf(stdout, "encrypted_private_key = %q\n", encrypted.Reveal())
	return nil
}

func runDecrypt(args []string, stdout io.Writer, stderr io.Writer, prompt prompt) error {
	var showPrivateKey bool
	fs := flag.NewFlagSet("keytool decrypt", flag.ContinueOnError)
	fs.BoolVar(&showPrivateKey, "show-private-key", false, "print the decrypted private key")
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("decrypt does not accept positional arguments")
	}

	encrypted, err := prompt.ReadLine("Enter encrypted_private_key: ")
	if err != nil {
		return fmt.Errorf("read encrypted private key: %w", err)
	}
	encrypted, err = normalizeEncryptedPrivateKeyInput(encrypted)
	if err != nil {
		return err
	}
	password, err := prompt.ReadSecret("Enter encryption password: ")
	if err != nil {
		return fmt.Errorf("read encryption password: %w", err)
	}
	privateKey, err := keycipher.Decrypt(secret.NewString(encrypted), password)
	if err != nil {
		return err
	}
	key, err := signing.ParsePrivateKey(privateKey)
	if err != nil {
		return err
	}
	address, err := key.Address()
	if err != nil {
		return err
	}

	fmt.Fprintln(stdout, "signer_address =", address)
	if showPrivateKey {
		fmt.Fprintf(stdout, "private_key = %q\n", privateKey.Reveal())
	}
	return nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  keytool encrypt")
	fmt.Fprintln(w, "  keytool decrypt [-show-private-key]")
}

func normalizeEncryptedPrivateKeyInput(value string) (string, error) {
	value = strings.TrimSpace(value)
	if before, after, ok := strings.Cut(value, "="); ok {
		if strings.TrimSpace(before) != "encrypted_private_key" {
			return "", fmt.Errorf("expected encrypted_private_key assignment")
		}
		value = strings.TrimSpace(after)
	}
	if unquoted, err := strconv.Unquote(value); err == nil {
		value = unquoted
	}
	if value == "" {
		return "", fmt.Errorf("encrypted_private_key is required")
	}
	return value, nil
}

func zeroBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
