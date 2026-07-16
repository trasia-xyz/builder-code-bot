package funding

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

const userRateLimitAlertThreshold uint64 = 200

type controlledAccount struct {
	kind    string
	address string
}

func rateLimitObservationContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	// Final observability must still run when cancellation is the funding task's
	// result. Individual Hyperliquid requests retain the HTTP client's timeout.
	return context.WithoutCancel(ctx)
}

func (o *Orchestrator) observeUserRateLimits(ctx context.Context) {
	accounts := make([]controlledAccount, 0, len(o.builders)+1)
	for _, builder := range o.builders {
		accounts = append(accounts, controlledAccount{kind: "builder", address: builder})
	}
	accounts = append(accounts, controlledAccount{kind: "settlement", address: o.settlement})

	for _, account := range accounts {
		limit, err := o.chain.UserRateLimit(ctx, account.address)
		if err != nil {
			o.warn(ctx, "Hyperliquid user rate limit query failed",
				slog.String("event", "funding_user_rate_limit_query_failed"),
				slog.String("account_kind", account.kind),
				slog.String("address", account.address))
			continue
		}

		remaining := limit.RemainingRequests()
		low := remaining < userRateLimitAlertThreshold
		attrs := []slog.Attr{
			slog.String("event", "funding_user_rate_limit_observed"),
			slog.String("account_kind", account.kind),
			slog.String("address", account.address),
			slog.Uint64("requests_remaining", remaining),
		}
		if low {
			o.warn(ctx, "Hyperliquid user rate limit is low", attrs...)
		} else {
			o.info(ctx, "Hyperliquid user rate limit observed", attrs...)
		}

		if o.updateRateLimitAlert(account.address, low) {
			o.alert(ctx, "user_rate_limit_low:"+strings.ToLower(strings.TrimSpace(account.address)),
				fmt.Sprintf(
					"Hyperliquid user rate limit is low\naccount kind: %s\naddress: %s\nrequests remaining: %d",
					account.kind, account.address, remaining,
				))
		}
	}
}

// updateRateLimitAlert returns true only when an account first enters the low
// state. A healthy observation clears the latch so a later regression alerts
// again, while funding retries do not send duplicate incident notifications.
func (o *Orchestrator) updateRateLimitAlert(address string, low bool) bool {
	key := strings.ToLower(strings.TrimSpace(address))
	o.alertMu.Lock()
	defer o.alertMu.Unlock()
	if !low {
		delete(o.lowLimits, key)
		return false
	}
	if _, exists := o.lowLimits[key]; exists {
		return false
	}
	if o.lowLimits == nil {
		o.lowLimits = make(map[string]struct{})
	}
	o.lowLimits[key] = struct{}{}
	return true
}
