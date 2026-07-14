package secret

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
)

func TestSecretStringRedactsCommonOutputPaths(t *testing.T) {
	secret := NewString("sensitive")
	if secret.Reveal() != "sensitive" || secret.String() != Masked || secret.GoString() != Masked {
		t.Fatalf("secret behavior = reveal %q, string %q, go %q", secret.Reveal(), secret.String(), secret.GoString())
	}
	if got := fmt.Sprintf("%v %#v", secret, secret); got != Masked+" "+Masked {
		t.Fatalf("formatted = %q", got)
	}
	data, err := json.Marshal(secret)
	if err != nil || string(data) != `"[MASKED]"` {
		t.Fatalf("json = %s, %v", data, err)
	}
	if got := secret.LogValue(); got.Kind() != slog.KindString || got.String() != Masked {
		t.Fatalf("log value = %v", got)
	}
}

func TestSecretStringUnmarshalText(t *testing.T) {
	var secret SecretString
	if err := secret.UnmarshalText([]byte("configured")); err != nil {
		t.Fatal(err)
	}
	if secret.Reveal() != "configured" {
		t.Fatalf("revealed = %q", secret.Reveal())
	}
}
