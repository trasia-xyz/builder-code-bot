package signing

import (
	"fmt"
	"testing"

	"hyperliquid-builder-code-bot/internal/secret"
)

func TestPrivateKeyAddress(t *testing.T) {
	key, err := ParsePrivateKey(secret.NewString("0x0000000000000000000000000000000000000000000000000000000000000001"))
	if err != nil {
		t.Fatalf("ParsePrivateKey() error = %v", err)
	}

	address, err := key.Address()
	if err != nil {
		t.Fatalf("Address() error = %v", err)
	}
	const want = "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf"
	if address != want {
		t.Fatalf("Address() = %q, want %q", address, want)
	}
}

func TestPrivateKeyIsMaskedWhenGoFormatted(t *testing.T) {
	key, err := ParsePrivateKey(secret.NewString("0x0000000000000000000000000000000000000000000000000000000000000001"))
	if err != nil {
		t.Fatalf("ParsePrivateKey() error = %v", err)
	}

	if got := fmt.Sprintf("%#v", key); got != secret.Masked {
		t.Fatalf("fmt.Sprintf(\"%%#v\", key) = %q, want %q", got, secret.Masked)
	}
}
