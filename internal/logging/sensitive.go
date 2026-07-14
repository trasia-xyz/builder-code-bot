package logging

import (
	"log/slog"
	"slices"
	"strings"
	"sync"
	"unicode"

	"builder-code-bot/internal/secret"
)

const redactedValue = secret.Masked

// defaultSensitiveKeys is a deliberately small, high-confidence safety net for
// raw values and untyped/external data (maps, third-party structs) that cannot
// carry a secret.Secret type. Prefer secret.Secret[T] / secret.SecretString at
// the data source; this list only catches values logged by key name. Keep it
// free of ambiguous words (e.g. "config", "seed", "signature") to avoid masking
// non-sensitive fields.
var defaultSensitiveKeys = []string{
	"api_key",
	"encrypted_private_key",
	"mnemonic",
	"passphrase",
	"password",
	"private_key",
	"secret",
	"signing_key",
	"access_token",
	"api_token",
	"auth_token",
	"bearer_token",
	"refresh_token",
	"session_token",
}

// sensitiveMatcher decides whether an attribute key looks sensitive. Both the
// configured patterns and candidate keys are split into lowercase tokens
// (snake_case, kebab-case, dotted, and camelCase are all normalized), and a key
// matches when a pattern's tokens appear as a contiguous run inside it. This
// avoids the false positives of plain substring matching (e.g. "config_path"
// is not masked by "config") while still catching variants such as "apiKey" or
// "signer_private_key".
type sensitiveMatcher struct {
	patterns [][]string
	// cache memoizes key -> sensitive decisions. The set of attribute keys a
	// program emits is small and bounded, so this stays tiny while removing the
	// per-key tokenization from the logging hot path. The pointer is shared
	// across handler clones (WithAttrs/WithGroup copy the struct by value).
	cache *sync.Map
}

func newSensitiveMatcher(keys []string) sensitiveMatcher {
	patterns := make([][]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		tokens := splitTokens(key)
		if len(tokens) == 0 {
			continue
		}
		fingerprint := strings.Join(tokens, "\x00")
		if _, ok := seen[fingerprint]; ok {
			continue
		}
		seen[fingerprint] = struct{}{}
		patterns = append(patterns, tokens)
	}
	return sensitiveMatcher{patterns: patterns, cache: &sync.Map{}}
}

func (m sensitiveMatcher) matches(key string) bool {
	if len(m.patterns) == 0 {
		return false
	}
	if m.cache != nil {
		if cached, ok := m.cache.Load(key); ok {
			return cached.(bool)
		}
	}
	result := m.matchUncached(key)
	if m.cache != nil {
		m.cache.Store(key, result)
	}
	return result
}

func (m sensitiveMatcher) matchUncached(key string) bool {
	tokens := splitTokens(key)
	if len(tokens) == 0 {
		return false
	}
	for _, pattern := range m.patterns {
		if containsTokens(tokens, pattern) {
			return true
		}
	}
	return false
}

// redact returns a masked copy of attr when its key looks sensitive, otherwise
// the original attr. Both the console and JSON paths funnel through here so the
// masking decision lives in a single place.
func (m sensitiveMatcher) redact(attr slog.Attr) slog.Attr {
	return m.redactKey(attr, attr.Key)
}

func (m sensitiveMatcher) redactKey(attr slog.Attr, key string) slog.Attr {
	if m.matches(key) {
		return slog.String(attr.Key, redactedValue)
	}
	return attr
}

func (m sensitiveMatcher) sanitizeAttrs(attrs []slog.Attr) []slog.Attr {
	if len(attrs) == 0 || len(m.patterns) == 0 {
		return attrs
	}
	var out []slog.Attr
	for i, attr := range attrs {
		next, changed := m.sanitizeAttr(nil, attr)
		if out == nil && changed {
			out = make([]slog.Attr, len(attrs))
			copy(out, attrs[:i])
		}
		if out != nil {
			out[i] = next
		}
	}
	if out == nil {
		return attrs
	}
	return out
}

func (m sensitiveMatcher) sanitizeAttr(groups []string, attr slog.Attr) (slog.Attr, bool) {
	if isEmptyAttr(attr) {
		return attr, false
	}
	if attr.Value.Kind() == slog.KindGroup {
		children := attr.Value.Group()
		if len(children) == 0 {
			return attr, false
		}
		nextGroups := appendGroupKey(groups, attr.Key)
		var out []slog.Attr
		for i, child := range children {
			next, changed := m.sanitizeAttr(nextGroups, child)
			if out == nil && changed {
				out = make([]slog.Attr, len(children))
				copy(out, children[:i])
			}
			if out != nil {
				out[i] = next
			}
		}
		if out == nil {
			return attr, false
		}
		return slog.Attr{Key: attr.Key, Value: slog.GroupValue(out...)}, true
	}
	if attr.Key != "" && m.matches(joinGroupKey(groups, attr.Key)) {
		return slog.String(attr.Key, redactedValue), true
	}
	return attr, false
}

func isEmptyAttr(attr slog.Attr) bool {
	return attr.Key == "" && attr.Value.Kind() == slog.KindAny && attr.Value.Any() == nil
}

func appendGroupKey(groups []string, key string) []string {
	if key == "" {
		return groups
	}
	next := make([]string, 0, len(groups)+1)
	next = append(next, groups...)
	next = append(next, key)
	return next
}

func joinGroupKey(groups []string, key string) string {
	if len(groups) == 0 {
		return key
	}
	if key == "" {
		return strings.Join(groups, ".")
	}
	var b strings.Builder
	for i, group := range groups {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(group)
	}
	b.WriteByte('.')
	b.WriteString(key)
	return b.String()
}

func containsTokens(haystack, needle []string) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if slices.Equal(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

// splitTokens lowercases s and splits it into tokens on separators
// (_ - . / : whitespace) and camelCase boundaries.
func splitTokens(s string) []string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	runes := []rune(s)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) {
			prev := runes[i-1]
			var next rune
			if i+1 < len(runes) {
				next = runes[i+1]
			}
			// camelCase ("apiKey") or acronym tail ("APIKey" -> "API","Key").
			if !unicode.IsUpper(prev) || unicode.IsLower(next) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(r)
	}
	return strings.FieldsFunc(strings.ToLower(b.String()), isTokenSeparator)
}

func isTokenSeparator(r rune) bool {
	switch r {
	case '_', '-', '.', '/', ':':
		return true
	default:
		return unicode.IsSpace(r)
	}
}
