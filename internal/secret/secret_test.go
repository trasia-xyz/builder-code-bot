package secret

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestSecretRedactsAcrossSinks(t *testing.T) {
	const raw = "super-secret-value"
	s := New(raw)

	outputs := map[string]string{
		"LogValue":    s.LogValue().String(),
		"String":      s.String(),
		"GoString":    s.GoString(),
		"fmt %s":      fmtFormat("%s", s),
		"fmt %v":      fmt.Sprintf("%v", s),
		"fmt %+v":     fmt.Sprintf("%+v", s),
		"fmt %#v":     fmt.Sprintf("%#v", s),
		"fmt %q":      fmt.Sprintf("%q", s),
		"MarshalText": string(mustMarshalText(t, s)),
	}
	for name, output := range outputs {
		assertMaskedNoLeak(t, name, output, raw)
	}

	encoded, err := s.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	if string(encoded) != `"`+Masked+`"` {
		t.Fatalf("MarshalJSON: got %s", encoded)
	}
	if s.Reveal() != raw {
		t.Fatal("Reveal must return the original value")
	}
}

func TestSecretStructJSONDoesNotLeak(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	payload := struct {
		Name string                    `json:"name"`
		Key  Secret[*ecdsa.PrivateKey] `json:"key"`
	}{
		Name: "signer",
		Key:  New(key),
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	out := string(encoded)
	if !strings.Contains(out, Masked) {
		t.Fatalf("expected masked key in %s", out)
	}
	if strings.Contains(out, key.D.String()) {
		t.Fatalf("private scalar leaked in %s", out)
	}
}

func TestSecretStructFmtDoesNotLeak(t *testing.T) {
	const raw = "super-secret-value"
	payload := struct {
		Name     string
		Key      Secret[string]
		Password SecretString
	}{
		Name:     "signer",
		Key:      New(raw),
		Password: NewString(raw),
	}

	outputs := map[string]string{
		"fmt %v":  fmt.Sprintf("%v", payload),
		"fmt %+v": fmt.Sprintf("%+v", payload),
		"fmt %#v": fmt.Sprintf("%#v", payload),
	}
	for name, output := range outputs {
		assertMaskedNoLeak(t, name, output, raw)
	}
}

func TestSecretStringRedactsAcrossSinks(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "short", value: "secret"},
		{name: "long", value: "0123456789abcdef"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewString(tt.value)
			outputs := map[string]string{
				"LogValue":    s.LogValue().String(),
				"String":      s.String(),
				"GoString":    s.GoString(),
				"fmt %s":      fmtFormat("%s", s),
				"fmt %v":      fmt.Sprintf("%v", s),
				"fmt %+v":     fmt.Sprintf("%+v", s),
				"fmt %#v":     fmt.Sprintf("%#v", s),
				"fmt %q":      fmt.Sprintf("%q", s),
				"MarshalText": string(mustMarshalText(t, s)),
			}
			for name, output := range outputs {
				assertMaskedNoLeak(t, name, output, tt.value)
			}

			encoded, err := s.MarshalJSON()
			if err != nil {
				t.Fatalf("marshal json: %v", err)
			}
			if string(encoded) != `"`+Masked+`"` {
				t.Fatalf("MarshalJSON: got %s", encoded)
			}
			if s.Reveal() != tt.value {
				t.Fatalf("Reveal: got %q want %q", s.Reveal(), tt.value)
			}
		})
	}
}

func TestSecretsRedactThroughSlogJSONHandler(t *testing.T) {
	const raw = "super-secret-value"
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	logger.Info("emit",
		slog.Any("generic", New(raw)),
		slog.Any("string", NewString(raw)),
	)

	output := buf.String()
	assertMaskedNoLeak(t, "slog JSON", output, raw)
	if got := strings.Count(output, Masked); got != 2 {
		t.Fatalf("slog JSON masked count = %d, want 2 in %s", got, output)
	}
}

func fmtFormat(format string, value any) string {
	var buf bytes.Buffer
	_, _ = fmt.Fprintf(&buf, format, value)
	return buf.String()
}

type textMarshaler interface {
	MarshalText() ([]byte, error)
}

func mustMarshalText(t *testing.T, value textMarshaler) []byte {
	t.Helper()
	text, err := value.MarshalText()
	if err != nil {
		t.Fatalf("marshal text: %v", err)
	}
	return text
}

func assertMaskedNoLeak(t *testing.T, name, output, raw string) {
	t.Helper()
	if !strings.Contains(output, Masked) {
		t.Fatalf("%s did not contain mask: %q", name, output)
	}
	if strings.Contains(output, raw) {
		t.Fatalf("%s leaked raw value %q in %q", name, raw, output)
	}
}
