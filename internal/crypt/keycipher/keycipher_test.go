package keycipher

import (
	"strings"
	"testing"

	"builder-code-bot/internal/secret"
)

const testKey = "0x0000000000000000000000000000000000000000000000000000000000000001"

func TestDecryptRejectsWrongPassword(t *testing.T) {
	ciphertext, err := Encrypt(secret.NewString(testKey), secret.NewString("correct"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Decrypt(ciphertext, secret.NewString("wrong"))
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("Decrypt() error = %v", err)
	}
}
