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

// Secret wraps a value of any type so it redacts itself when logged, formatted,
// or serialized. Retrieve the underlying value explicitly with Reveal when
// business logic genuinely needs it.
type Secret[T any] struct {
	value T
}

// New wraps value in a Secret.
func New[T any](value T) Secret[T] {
	return Secret[T]{value: value}
}

// Reveal returns the underlying value. Each call is an explicit, greppable
// signal that a secret is being used in the clear.
func (s Secret[T]) Reveal() T {
	return s.value
}

// LogValue implements slog.LogValuer.
func (Secret[T]) LogValue() slog.Value {
	return slog.StringValue(Masked)
}

// String implements fmt.Stringer.
func (Secret[T]) String() string {
	return Masked
}

// GoString implements fmt.GoStringer.
func (Secret[T]) GoString() string {
	return Masked
}

// MarshalJSON implements json.Marshaler.
func (Secret[T]) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(Masked)), nil
}

// MarshalText implements encoding.TextMarshaler.
func (Secret[T]) MarshalText() ([]byte, error) {
	return []byte(Masked), nil
}

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

var (
	_ slog.LogValuer = Secret[any]{}
	_ fmt.Stringer   = Secret[any]{}
	_ fmt.GoStringer = Secret[any]{}
	_ json.Marshaler = Secret[any]{}

	_ slog.LogValuer = SecretString{}
	_ fmt.Stringer   = SecretString{}
	_ fmt.GoStringer = SecretString{}
	_ json.Marshaler = SecretString{}
)
