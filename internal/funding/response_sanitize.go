package funding

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"hyperliquid-builder-code-bot/internal/hyperliquid/exchange"
)

func (o *Orchestrator) logSubmitResult(ctx context.Context, runID string, result exchange.SubmitResult) {
	attrs := []slog.Attr{
		slog.String("event", "funding_payout_submit_result"),
		slog.String("run_id", runID),
		slog.Bool("accepted", result.Accepted),
		slog.Bool("rejected", result.Rejected),
	}
	if response := sanitizeJSON(result.Response); len(response) != 0 {
		attrs = append(attrs, slog.String("response", string(response)))
	}
	o.info(ctx, "final payout submission returned", attrs...)
}

func sanitizeJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		data, _ := json.Marshal(map[string]string{"unparseable_response": "redacted"})
		return data
	}
	redactSensitive(value)
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}

func redactSensitive(value any) {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			if sensitiveResponseKey(key) {
				current[key] = "[REDACTED]"
				continue
			}
			redactSensitive(child)
		}
	case []any:
		for _, child := range current {
			redactSensitive(child)
		}
	}
}

func sensitiveResponseKey(key string) bool {
	normalized := canonicalResponseKey(key)
	for _, sensitive := range []string{"private_key", "encrypted_private_key", "password", "passphrase", "mnemonic", "secret", "signing_key", "access_token", "api_token", "auth_token", "bearer_token", "refresh_token", "session_token"} {
		canonicalSensitive := canonicalResponseKey(sensitive)
		if normalized == canonicalSensitive || strings.HasSuffix(normalized, canonicalSensitive) {
			return true
		}
	}
	return false
}

func canonicalResponseKey(key string) string {
	var normalized strings.Builder
	for _, char := range strings.ToLower(key) {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			normalized.WriteRune(char)
		}
	}
	return normalized.String()
}
