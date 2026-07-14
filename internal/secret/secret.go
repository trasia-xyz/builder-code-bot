// Package secret provides types that carry sensitive material and redact common
// accidental output paths. Values wrapped here redact themselves when logged,
// formatted, or serialized; the underlying value is only available through an
// explicit Reveal call.
//
// The redaction travels with the data, not with any particular log call site:
// declare a struct field as secret.Secret[T] once and every downstream sink
// (slog, fmt, encoding/json) renders it as Masked automatically.
//
// This package is not a secure-memory container: it does not own encryption,
// key management, memory zeroing, or the lifetime of plaintext values after
// Reveal returns them.
package secret

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
)

// Masked is the placeholder rendered in place of any secret value.
const Masked = "[MASKED]"

// SecretString carries a sensitive string and redacts itself when logged,
// formatted, or serialized.
type SecretString struct {
	value string
}

// NewString wraps value in a SecretString.
func NewString(value string) SecretString {
	return SecretString{value: value}
}

// Reveal returns the underlying string.
func (s SecretString) Reveal() string {
	return s.value
}

// LogValue implements slog.LogValuer.
func (SecretString) LogValue() slog.Value {
	return slog.StringValue(Masked)
}

// String implements fmt.Stringer.
func (SecretString) String() string {
	return Masked
}

// GoString implements fmt.GoStringer.
func (SecretString) GoString() string {
	return Masked
}

// MarshalJSON implements json.Marshaler.
func (SecretString) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(Masked)), nil
}

// MarshalText implements encoding.TextMarshaler.
func (SecretString) MarshalText() ([]byte, error) {
	return []byte(Masked), nil
}

// UnmarshalText decodes a secret from strict text-based configuration formats.
func (s *SecretString) UnmarshalText(value []byte) error {
	s.value = string(value)
	return nil
}

var (
	_ slog.LogValuer = SecretString{}
	_ fmt.Stringer   = SecretString{}
	_ fmt.GoStringer = SecretString{}
	_ json.Marshaler = SecretString{}
)
