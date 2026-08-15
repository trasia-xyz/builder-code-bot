package funding

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"builder-code-bot/internal/logging"
)

const (
	userRateLimitAlertThreshold uint64 = 200

	// This small window covers the bounded burst of actions one controlled
	// account can issue in a run. A first response inside it may still be a
	// pre-action snapshot that hides a value below the alert threshold.
	userRateLimitVisibilityActionWindow uint64 = 6
	userRateLimitVisibilityAttempts            = 3
	userRateLimitVisibilityInterval            = time.Second
)

type controlledAccount struct {
	kind    string
	address string
}

type userRateLimitObservation struct {
	remaining             uint64
	attempts              int
	visibilityUnconfirmed bool
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
		observation, err := o.observeUserRateLimit(
			ctx, account, report.submitted(account.address),
		)
		if err != nil {
			if builderReport := report.builder(account.address); builderReport != nil {
				builderReport.RateLimitQueryFailed = true
			} else if report != nil && account.kind == "settlement" {
				report.SettlementRateLimitQueryFailed = true
			}
			report.addWarning(accountLabel(account, report) + ": Rate Limit 查询失败")
			o.warn(ctx, "Hyperliquid user rate limit query failed",
				slog.String("event", "funding_user_rate_limit_query_failed"),
				slog.String("account_kind", account.kind),
				slog.String("address", account.address),
				logging.Err(err))
			continue
		}

		remaining := observation.remaining
		if builderReport := report.builder(account.address); builderReport != nil {
			builderReport.RateLimitObserved = true
			builderReport.RateLimitRemaining = remaining
			builderReport.RateLimitVisibilityUnconfirmed = observation.visibilityUnconfirmed
		} else if report != nil && account.kind == "settlement" {
			report.SettlementRateLimitObserved = true
			report.SettlementRateLimitRemaining = remaining
			report.SettlementRateLimitVisibilityUnconfirmed = observation.visibilityUnconfirmed
		}
		low := remaining < userRateLimitAlertThreshold
		attrs := []slog.Attr{
			slog.String("event", "funding_user_rate_limit_observed"),
			slog.String("account_kind", account.kind),
			slog.String("address", account.address),
			slog.Uint64("requests_remaining", remaining),
			slog.Int("observation_attempts", observation.attempts),
			slog.Bool("visibility_unconfirmed", observation.visibilityUnconfirmed),
		}
		switch {
		case low:
			o.warn(ctx, "Hyperliquid user rate limit is low", attrs...)
			report.addWarning(fmt.Sprintf("%s: Rate Limit 仅剩 %d", accountLabel(account, report), remaining))
		case observation.visibilityUnconfirmed:
			attrs[0] = slog.String("event", "funding_user_rate_limit_visibility_unconfirmed")
			o.warn(ctx, "Hyperliquid user rate limit post-action visibility is unconfirmed", attrs...)
			report.addWarning(fmt.Sprintf(
				"%s: Rate Limit 动作后计数未确认刷新（最低观测值 %d）",
				accountLabel(account, report), remaining,
			))
		default:
			o.info(ctx, "Hyperliquid user rate limit observed", attrs...)
		}

		atRisk := low || observation.visibilityUnconfirmed
		if o.updateRateLimitAlert(account.address, atRisk) {
			message := fmt.Sprintf(
				"Hyperliquid user rate limit is low\naccount kind: %s\naddress: %s\nrequests remaining: %d",
				account.kind, account.address, remaining,
			)
			if observation.visibilityUnconfirmed {
				message = fmt.Sprintf(
					"Hyperliquid user rate limit may be low because post-action visibility was not confirmed\naccount kind: %s\naddress: %s\nlowest observed remaining: %d",
					account.kind, account.address, remaining,
				)
			}
			o.alert(
				ctx, "user_rate_limit_low:"+strings.ToLower(strings.TrimSpace(account.address)),
				AlertSeverityWarning,
				message,
			)
		}
	}
}

func (o *Orchestrator) observeUserRateLimit(
	ctx context.Context,
	account controlledAccount,
	actionSubmitted bool,
) (userRateLimitObservation, error) {
	limit, err := o.chain.UserRateLimit(ctx, account.address)
	if err != nil {
		return userRateLimitObservation{}, err
	}
	observation := userRateLimitObservation{
		remaining: limit.RemainingRequests(), attempts: 1,
	}
	if !actionSubmitted || !rateLimitNeedsVisibilityConfirmation(observation.remaining) {
		return observation, nil
	}

	for attempt := 2; attempt <= userRateLimitVisibilityAttempts; attempt++ {
		if err := o.sleeper.Sleep(ctx, userRateLimitVisibilityInterval); err != nil {
			observation.visibilityUnconfirmed = true
			o.warn(ctx, "Hyperliquid user rate limit visibility wait interrupted",
				slog.String("event", "funding_user_rate_limit_visibility_wait_interrupted"),
				slog.String("account_kind", account.kind),
				slog.String("address", account.address),
				slog.Int("attempt", attempt),
				slog.Int("max_attempts", userRateLimitVisibilityAttempts),
				logging.Err(err))
			return observation, nil
		}

		limit, err = o.chain.UserRateLimit(ctx, account.address)
		observation.attempts = attempt
		if err != nil {
			observation.visibilityUnconfirmed = true
			o.warn(ctx, "Hyperliquid user rate limit visibility query failed",
				slog.String("event", "funding_user_rate_limit_visibility_query_failed"),
				slog.String("account_kind", account.kind),
				slog.String("address", account.address),
				slog.Int("attempt", attempt),
				slog.Int("max_attempts", userRateLimitVisibilityAttempts),
				logging.Err(err))
			return observation, nil
		}
		remaining := limit.RemainingRequests()
		if remaining < observation.remaining {
			observation.remaining = remaining
		}
		if observation.remaining < userRateLimitAlertThreshold {
			return observation, nil
		}
	}

	// Repeated near-threshold snapshots cannot prove that the action request
	// count is visible. Keep the lowest value, but surface the uncertainty and
	// alert conservatively instead of presenting the stale boundary as healthy.
	observation.visibilityUnconfirmed = true
	return observation, nil
}

func rateLimitNeedsVisibilityConfirmation(remaining uint64) bool {
	return remaining >= userRateLimitAlertThreshold &&
		remaining < userRateLimitAlertThreshold+userRateLimitVisibilityActionWindow
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

// updateRateLimitAlert returns true only when an account first enters a low or
// conservatively at-risk state. A healthy observation clears the latch so a
// later regression alerts again, while funding retries do not send duplicate
// incident notifications.
func (o *Orchestrator) updateRateLimitAlert(address string, atRisk bool) bool {
	key := strings.ToLower(strings.TrimSpace(address))
	o.alertMu.Lock()
	defer o.alertMu.Unlock()
	if !atRisk {
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
