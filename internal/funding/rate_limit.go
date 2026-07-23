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
	// Final observation after non-cancellation outcomes must not inherit an
	// already-expired task deadline. Callers skip observation when cancellation
	// itself is the funding result. Individual requests retain the HTTP timeout.
	return context.WithoutCancel(ctx)
}

func (o *Orchestrator) observeUserRateLimits(ctx context.Context, report *RunReport) {
	accounts := make([]controlledAccount, 0, len(o.builders)+1)
	for _, builder := range o.builders {
		accounts = append(accounts, controlledAccount{kind: "builder", address: builder})
	}
	settlement := o.settlement
	if report != nil && strings.TrimSpace(report.SettlementBalance.Address) != "" {
		settlement = report.SettlementBalance.Address
	}
	accounts = append(accounts, controlledAccount{kind: "settlement", address: settlement})

	for _, account := range accounts {
		limit, err := o.chain.UserRateLimit(ctx, account.address)
		if err != nil {
			if builderReport := report.builder(account.address); builderReport != nil {
				builderReport.RateLimitQueryFailed = true
			} else if report != nil && account.kind == "settlement" {
				report.SettlementRateLimitQueryFailed = true
			}
			report.addWarning(accountLabel(account, report) + "：Rate Limit 查询失败")
			o.warn(ctx, "Hyperliquid user rate limit query failed",
				slog.String("event", "funding_user_rate_limit_query_failed"),
				slog.String("account_kind", account.kind),
				slog.String("address", account.address))
			continue
		}

		remaining := limit.RemainingRequests()
		if builderReport := report.builder(account.address); builderReport != nil {
			builderReport.RateLimitObserved = true
			builderReport.RateLimitRemaining = remaining
		} else if report != nil && account.kind == "settlement" {
			report.SettlementRateLimitObserved = true
			report.SettlementRateLimitRemaining = remaining
		}
		low := remaining < userRateLimitAlertThreshold
		attrs := []slog.Attr{
			slog.String("event", "funding_user_rate_limit_observed"),
			slog.String("account_kind", account.kind),
			slog.String("address", account.address),
			slog.Uint64("requests_remaining", remaining),
		}
		if low {
			o.warn(ctx, "Hyperliquid user rate limit is low", attrs...)
			report.addWarning(fmt.Sprintf("%s：Rate Limit 仅剩 %d", accountLabel(account, report), remaining))
		} else {
			o.info(ctx, "Hyperliquid user rate limit observed", attrs...)
		}

		if o.updateRateLimitAlert(account.address, low) {
			o.alert(
				ctx, "user_rate_limit_low:"+strings.ToLower(strings.TrimSpace(account.address)),
				AlertSeverityWarning,
				fmt.Sprintf(
					"Hyperliquid user rate limit is low\naccount kind: %s\naddress: %s\nrequests remaining: %d",
					account.kind, account.address, remaining,
				),
			)
		}
	}
}

func accountLabel(account controlledAccount, report *RunReport) string {
	if builder := report.builder(account.address); builder != nil {
		return builder.Name
	}
	if account.kind == "settlement" {
		return "Settlement"
	}
	return account.kind
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
