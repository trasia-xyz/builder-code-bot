package funding

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const (
	settlementFinalBalanceObservationAttempts = 5
	settlementFinalBalanceQueryAttempts       = 20
	settlementFinalBalanceObservationInterval = time.Second
	settlementFinalBalanceConfirmations       = 2
)

func (o *Orchestrator) observeSettlementFinalBalance(ctx context.Context, report *RunReport) {
	if o == nil || report == nil {
		return
	}
	// An ambiguous payout may already have moved funds. Its bounded observer has
	// already failed to establish the exact ending boundary, so a later single
	// read must not be relabelled as the task's ending balance.
	if report.SettlementBalance.PostPayoutUnconfirmed &&
		report.Payout.Status != ReportStepSuccess {
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
	expectedTotal, requirePostPayout, boundaryErr := postPayoutBalanceBoundary(report)
	if boundaryErr != nil {
		o.markSettlementFinalBalanceBoundaryInvalid(ctx, report, settlement, boundaryErr)
		return
	}
	if !requirePostPayout {
		balance, err := o.chain.SpotBalance(ctx, settlement, *token)
		if err != nil {
			o.markSettlementFinalBalanceQueryFailed(ctx, report, settlement)
			return
		}
		recordSettlementFinalBalance(report, settlement, balance.Total, balance.Available)
		return
	}

	var (
		lastTotal                string
		candidateAvailable       decimal.Decimal
		observedAny              bool
		successfulObservations   int
		successfulLimit          = settlementFinalBalanceObservationAttempts
		consecutiveConfirmations int
	)
	for queryAttempt := 1; queryAttempt <= settlementFinalBalanceQueryAttempts &&
		successfulObservations < successfulLimit; queryAttempt++ {
		if err := o.sleeper.Sleep(ctx, settlementFinalBalanceObservationInterval); err != nil {
			o.markSettlementFinalBalanceQueryFailed(ctx, report, settlement)
			return
		}
		balance, err := o.chain.SpotBalance(ctx, settlement, *token)
		if err != nil {
			consecutiveConfirmations = 0
			o.warn(ctx, "post-payout settlement balance query failed",
				slog.String("event", "funding_settlement_post_payout_balance_query_failed"),
				slog.String("settlement", settlement),
				slog.Int("query_attempt", queryAttempt),
				slog.Int("successful_observations", successfulObservations))
			continue
		}

		observedAny = true
		successfulObservations++
		lastTotal = balance.Total.String()
		matchesExpected := balance.Total.Equal(expectedTotal)
		if matchesExpected {
			if consecutiveConfirmations > 0 && balance.Available.Equal(candidateAvailable) {
				consecutiveConfirmations++
			} else {
				candidateAvailable = balance.Available
				consecutiveConfirmations = 1
			}
			// Reserve another successful observation when the expected value
			// first appears at the end of the ordinary visibility window.
			if consecutiveConfirmations < settlementFinalBalanceConfirmations &&
				successfulObservations >= successfulLimit &&
				successfulLimit < settlementFinalBalanceQueryAttempts {
				successfulLimit++
			}
		} else {
			consecutiveConfirmations = 0
		}
		if consecutiveConfirmations >= settlementFinalBalanceConfirmations {
			recordSettlementFinalBalance(report, settlement, balance.Total, balance.Available)
			hold := balance.Total.Sub(balance.Available)
			o.info(ctx, "final settlement balance observed",
				slog.String("event", "funding_settlement_final_balance_observed"),
				slog.String("settlement", settlement),
				slog.String("total", balance.Total.String()),
				slog.String("hold", hold.String()),
				slog.String("available", balance.Available.String()),
				slog.Int("query_attempt", queryAttempt),
				slog.Int("successful_observations", successfulObservations),
				slog.Int("consecutive_confirmations", consecutiveConfirmations))
			return
		}
		o.info(ctx, "post-payout settlement balance is not visible yet",
			slog.String("event", "funding_settlement_post_payout_balance_pending"),
			slog.String("settlement", settlement),
			slog.String("expected_total", expectedTotal.String()),
			slog.String("total", balance.Total.String()),
			slog.String("available", balance.Available.String()),
			slog.Int("query_attempt", queryAttempt),
			slog.Int("successful_observations", successfulObservations),
			slog.Int("successful_limit", successfulLimit),
			slog.Int("consecutive_confirmations", consecutiveConfirmations))
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
		slog.String("expected_total", expectedTotal.String()),
		slog.String("last_total", lastTotal),
		slog.Int("successful_observations", successfulObservations))
}

func markSettlementPayoutUnconfirmed(report *RunReport) {
	if report == nil {
		return
	}
	address := report.SettlementBalance.Address
	if strings.TrimSpace(address) == "" {
		address = report.Payout.Settlement
	}
	report.SettlementBalance = SettlementBalanceReport{
		Address: address, PostPayoutUnconfirmed: true,
	}
}

func postPayoutBalanceBoundary(report *RunReport) (decimal.Decimal, bool, error) {
	if report == nil || report.Payout.Status != ReportStepSuccess {
		return decimal.Zero, false, nil
	}
	amount, amountErr := decimal.NewFromString(report.Payout.Amount)
	totalBefore, beforeErr := decimal.NewFromString(report.Payout.SettlementTotalBefore)
	if amountErr != nil || beforeErr != nil || !amount.IsPositive() {
		return decimal.Zero, true, fmt.Errorf("invalid successful payout balance boundary")
	}
	expected := totalBefore.Sub(amount)
	if expected.IsNegative() {
		return decimal.Zero, true, fmt.Errorf("negative successful payout balance boundary")
	}
	return expected, true, nil
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

func (o *Orchestrator) markSettlementFinalBalanceBoundaryInvalid(
	ctx context.Context,
	report *RunReport,
	settlement string,
	err error,
) {
	report.SettlementBalance = SettlementBalanceReport{
		Address: settlement, PostPayoutUnconfirmed: true,
	}
	report.addWarning("Settlement: Payout 已成功但付款后余额边界无效，任务结束余额无法确认")
	o.warn(ctx, "successful payout settlement balance boundary is invalid",
		slog.String("event", "funding_settlement_final_balance_boundary_invalid"),
		slog.String("settlement", settlement),
		slog.String("error", err.Error()))
}
