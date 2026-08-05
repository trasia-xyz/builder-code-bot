package funding

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const (
	settlementFinalBalanceObservationAttempts = 5
	settlementFinalBalanceObservationInterval = time.Second
)

func (o *Orchestrator) observeSettlementFinalBalance(ctx context.Context, report *RunReport) {
	if o == nil || report == nil {
		return
	}
	token := report.token
	if token == nil {
		resolved, err := o.chain.CanonicalUSDC(ctx)
		if err != nil {
			report.SettlementBalance.QueryFailed = true
			report.addWarning("Settlement: 任务结束余额查询失败")
			o.warn(ctx, "final settlement token query failed",
				slog.String("event", "funding_settlement_final_balance_query_failed"),
				slog.String("query_kind", "canonical_usdc"))
			return
		}
		token = &resolved
		report.token = cloneToken(token)
	}

	settlement := strings.TrimSpace(report.SettlementBalance.Address)
	if settlement == "" {
		settlement = o.settlement
	}
	totalBefore, requirePostPayout := postPayoutBalanceBoundary(report)
	attempts := 1
	if requirePostPayout {
		attempts = settlementFinalBalanceObservationAttempts
	}

	var lastTotal string
	observedAny := false
	for attempt := 1; attempt <= attempts; attempt++ {
		balance, err := o.chain.SpotBalance(ctx, settlement, *token)
		if err == nil {
			observedAny = true
			lastTotal = balance.Total.String()
			if !requirePostPayout || balance.Total.LessThan(totalBefore) {
				recordSettlementFinalBalance(report, settlement, balance.Total, balance.Available)
				hold := balance.Total.Sub(balance.Available)
				o.info(ctx, "final settlement balance observed",
					slog.String("event", "funding_settlement_final_balance_observed"),
					slog.String("settlement", settlement),
					slog.String("total", balance.Total.String()),
					slog.String("hold", hold.String()),
					slog.String("available", balance.Available.String()),
					slog.Int("attempt", attempt),
					slog.Bool("post_payout", requirePostPayout))
				return
			}
			o.info(ctx, "post-payout settlement balance is not visible yet",
				slog.String("event", "funding_settlement_post_payout_balance_pending"),
				slog.String("settlement", settlement),
				slog.String("total_before", totalBefore.String()),
				slog.String("total", balance.Total.String()),
				slog.Int("attempt", attempt),
				slog.Int("max_attempts", attempts))
		} else if !requirePostPayout {
			o.markSettlementFinalBalanceQueryFailed(ctx, report, settlement)
			return
		}

		if attempt < attempts {
			if err := o.sleeper.Sleep(ctx, settlementFinalBalanceObservationInterval); err != nil {
				o.markSettlementFinalBalanceQueryFailed(ctx, report, settlement)
				return
			}
		}
	}

	if !observedAny {
		o.markSettlementFinalBalanceQueryFailed(ctx, report, settlement)
		return
	}
	report.SettlementBalance = SettlementBalanceReport{
		Address: settlement, PostPayoutUnconfirmed: true,
	}
	report.addWarning("Settlement: Payout 后余额未在有限观察内更新，任务结束余额无法确认")
	o.warn(ctx, "post-payout settlement balance remained unconfirmed",
		slog.String("event", "funding_settlement_post_payout_balance_unconfirmed"),
		slog.String("settlement", settlement),
		slog.String("total_before", totalBefore.String()),
		slog.String("last_total", lastTotal),
		slog.Int("attempts", attempts))
}

func postPayoutBalanceBoundary(report *RunReport) (decimal.Decimal, bool) {
	if report == nil || report.Payout.Status != ReportStepSuccess {
		return decimal.Zero, false
	}
	amount, amountErr := decimal.NewFromString(report.Payout.Amount)
	totalBefore, beforeErr := decimal.NewFromString(report.Payout.SettlementTotalBefore)
	if amountErr != nil || beforeErr != nil || !amount.IsPositive() {
		return decimal.Zero, false
	}
	return totalBefore, true
}

func recordSettlementFinalBalance(
	report *RunReport,
	settlement string,
	total, available decimal.Decimal,
) {
	hold := total.Sub(available)
	report.SettlementBalance = SettlementBalanceReport{
		Address: settlement, Total: total.String(), Hold: hold.String(),
		Available: available.String(), Observed: true,
	}
}

func (o *Orchestrator) markSettlementFinalBalanceQueryFailed(
	ctx context.Context,
	report *RunReport,
	settlement string,
) {
	report.SettlementBalance = SettlementBalanceReport{Address: settlement, QueryFailed: true}
	report.addWarning("Settlement: 任务结束余额查询失败")
	o.warn(ctx, "final settlement balance query failed",
		slog.String("event", "funding_settlement_final_balance_query_failed"),
		slog.String("query_kind", "spot_balance"),
		slog.String("settlement", settlement))
}
