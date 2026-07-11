package signing

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"hyperliquid-builder-code-bot/internal/secret"
)

// PrivateKey wraps an Ethereum secp256k1 private key and masks itself when
// logged or formatted.
type PrivateKey struct {
	key *secp256k1.PrivateKey
}

// ParsePrivateKey parses a 32-byte hex private key.
func ParsePrivateKey(value secret.SecretString) (PrivateKey, error) {
	hexKey := strings.TrimPrefix(value.Reveal(), "0x")
	if len(hexKey) != 64 {
		return PrivateKey{}, fmt.Errorf("private key must be 32 bytes")
	}
	decoded, err := hex.DecodeString(hexKey)
	if err != nil {
		return PrivateKey{}, fmt.Errorf("parse private key: %w", err)
	}
	var raw [32]byte
	copy(raw[:], decoded)
	var scalar secp256k1.ModNScalar
	overflow := scalar.SetBytes(&raw)
	if overflow != 0 || scalar.IsZero() {
		return PrivateKey{}, fmt.Errorf("private key is outside secp256k1 range")
	}
	return PrivateKey{key: secp256k1.NewPrivateKey(&scalar)}, nil
}

// Address returns the EIP-55 Ethereum address derived from the private key.
func (k PrivateKey) Address() (string, error) {
	if k.key == nil {
		return "", fmt.Errorf("private key is empty")
	}
	publicKey := k.key.PubKey().SerializeUncompressed()
	hash := keccak256(publicKey[1:])
	return checksumAddress(hash[12:]), nil
}

func (k PrivateKey) require() (*secp256k1.PrivateKey, error) {
	if k.key == nil {
		return nil, fmt.Errorf("private key is empty")
	}
	return k.key, nil
}

func checksumAddress(address []byte) string {
	lower := hex.EncodeToString(address)
	hash := keccak256([]byte(lower))

	out := []byte(lower)
	for i := range out {
		if out[i] >= '0' && out[i] <= '9' {
			continue
		}
		nibble := hash[i/2] >> 4
		if i%2 == 1 {
			nibble = hash[i/2] & 0x0f
		}
		if nibble >= 8 {
			out[i] -= 'a' - 'A'
		}
	}
	return "0x" + string(out)
}

// String implements fmt.Stringer.
func (PrivateKey) String() string {
	return secret.Masked
}

// GoString implements fmt.GoStringer.
func (PrivateKey) GoString() string {
	return secret.Masked
}

// LogValue implements slog.LogValuer.
func (PrivateKey) LogValue() slog.Value {
	return slog.StringValue(secret.Masked)
}

var _ slog.LogValuer = PrivateKey{}
