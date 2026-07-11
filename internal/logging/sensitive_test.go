package logging

import (
	"log/slog"
	"testing"
)

func TestSensitiveMatcherTokenMatching(t *testing.T) {
	matcher := newSensitiveMatcher([]string{"api_key", "private_key", "secret", "  wallet  "})

	redacted := []string{
		"api_key",
		"apiKey",           // camelCase variant
		"exchange_api_key", // pattern as a contiguous run
		"signer_private_key",
		"privateKey",
		"client_secret",
		"wallet_address",
	}
	for _, key := range redacted {
		if !matcher.matches(key) {
			t.Errorf("expected %q to be treated as sensitive", key)
		}
	}

	visible := []string{
		"config_path", // "config" no longer in the dictionary, and not a token here
		"seed_count",
		"signature",
		"secretary", // substring of "secret" but a different token
		"api_request_id",
		"safe",
	}
	for _, key := range visible {
		if matcher.matches(key) {
			t.Errorf("expected %q to pass through", key)
		}
	}
}

func TestSensitiveMatcherCachesDecisions(t *testing.T) {
	m := newSensitiveMatcher([]string{"api_key"})

	if !m.matches("api_key") {
		t.Fatal("expected api_key to match")
	}
	if v, ok := m.cache.Load("api_key"); !ok || v.(bool) != true {
		t.Fatalf("expected a positive decision to be cached, got ok=%v v=%v", ok, v)
	}

	if m.matches("safe") {
		t.Fatal("expected safe to pass through")
	}
	if v, ok := m.cache.Load("safe"); !ok || v.(bool) != false {
		t.Fatalf("expected a negative decision to be cached, got ok=%v v=%v", ok, v)
	}

	// The cached fast path must always agree with the uncached computation,
	// and a second lookup (cache hit) must return the same result.
	for _, key := range []string{"api_key", "safe", "exchange_api_key", "note", "apiKey"} {
		first := m.matches(key) // populates the cache
		if first != m.matchUncached(key) {
			t.Fatalf("cached and uncached results diverged for %q", key)
		}
		if second := m.matches(key); second != first { // cache hit
			t.Fatalf("repeated lookup changed result for %q", key)
		}
	}
}

func TestSensitiveMatcherWithoutPatternsNeverMatches(t *testing.T) {
	m := newSensitiveMatcher(nil)
	if m.matches("api_key") {
		t.Fatal("matcher without patterns must not match anything")
	}
	// The empty-pattern short circuit must run before the cache is touched, so
	// it never accumulates entries.
	if _, ok := m.cache.Load("api_key"); ok {
		t.Fatal("empty matcher must not populate the cache")
	}
}

func TestSensitiveMatcherRedact(t *testing.T) {
	m := newSensitiveMatcher([]string{"secret"})

	masked := m.redact(slog.String("client_secret", "value"))
	if masked.Key != "client_secret" || masked.Value.String() != redactedValue {
		t.Fatalf("expected sensitive attr to be masked, got key=%q value=%q", masked.Key, masked.Value.String())
	}

	passthrough := m.redact(slog.String("note", "value"))
	if passthrough.Value.String() != "value" {
		t.Fatalf("expected non-sensitive attr to pass through, got %q", passthrough.Value.String())
	}
}
