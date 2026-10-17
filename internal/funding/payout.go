package funding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"builder-code-bot/internal/hyperliquid/info"
	"builder-code-bot/internal/logging"

	"github.com/shopspring/decimal"
)

const (
	// Ambiguous payouts wait before every query, including the first one. With
	// no query failures this covers ten full seconds instead of spending the
	// first observation immediately after Submit returns. Query failures use the
	// larger request budget without consuming the successful-observation budget.
	payoutBalanceObservationAttempts      = 10
	payoutBalanceQueryAttempts            = 20
	payoutBalanceRequiredConfirmations    = 2
	payoutBalanceObservationInterval      = time.Second
	uncertainPayoutBlockedReasonPrefix    = "final payout result remained uncertain"
	explicitlyRejectedPayoutBlockedReason = "final payout was explicitly rejected"
)

var payoutNewAddressActivationFee = decimal.NewFromInt(1)

// FatalError marks a funding result that must terminate the long-running
// process. Restarting remains fail-closed because the blocked current state is
// retained and recovered before any new run can begin.
type FatalError struct{ Err error }

func (e *FatalError) Error() string { return e.Err.Error() }
func (e *FatalError) Unwrap() error { return e.Err }
func (*FatalError) Fatal()          {}

func IsFatal(err error) bool {
	var fatal *FatalError
	return errors.As(err, &fatal)
}

func (o *Orchestrator) executePayout(
	ctx context.Context,
	state *RunState,
	target *PayoutTargetState,
	before info.SpotBalanceAmounts,
	amount decimal.Decimal,
	report *RunReport,
) error {
	targetReport := report.target(target.Kind)
	targetReport.Payout.Amount = amount.String()
	targetReport.Payout.SettlementTotalBefore = before.Total.String()
	o.info(ctx, "final payout preparation started",
		slog.String("event", "funding_payout_preparation_started"),
		slog.String("run_id", state.RunID), slog.String("payout_total", amount.String()),
		slog.String("payout_kind", string(target.Kind)),
		slog.String("settlement_total_before", before.Total.String()),
		slog.String("recipient", target.Manifest.Recipient))
	prepared, err := o.chain.PrepareSpotSend(
		target.Manifest.Settlement, target.Manifest.Recipient, *target.Manifest.Token, amount, o.nonce.Next(),
	)
	if err != nil {
		o.alertOnce(
			ctx, state.RunID, "payout_prepare_failed",
			AlertSeverityWarning, "final payout preparation failed",
		)
		report.failTarget(target.Kind, "payout", "Payout 请求准备失败。", "请检查 Settlement signer、Token 和收款地址配置。")
		return err
	}
	if err := validatePreparedPayout(target, prepared); err != nil {
		report.failTarget(target.Kind, "payout", "Payout 请求未通过安全校验。", "请保留现场并检查不可变 Manifest 和签名请求。")
		return err
	}
	targetReport.Payout.Nonce = prepared.Nonce
	target.Payout = &PayoutJournal{
		Prepared: clonePrepared(prepared), TotalBefore: before.Total.String(), SubmitAttempts: 1,
	}
	if err := o.saveActivePhase(ctx, state, PhasePayoutPrepared); err != nil {
		report.failTarget(target.Kind, "payout", "Payout 意图持久化失败，付款未开始。", "请检查 data 目录和文件系统状态。")
		return err
	}
	report.syncState(state)
	return o.submitPreparedPayout(ctx, state, target, report)
}

func (o *Orchestrator) submitPreparedPayout(
	ctx context.Context,
	state *RunState,
	target *PayoutTargetState,
	report *RunReport,
) error {
	if err := validateTargetProgress(state, target); err != nil {
		report.failTarget(target.Kind, "payout", "Payout 目标状态未通过安全校验。", "请保留 current 状态，禁止直接重发。")
		return &FatalError{Err: err}
	}
	if err := validatePayoutJournal(target); err != nil {
		report.failTarget(target.Kind, "payout", "持久化的 Payout journal 未通过安全校验。", "请保留 current 状态，禁止直接重发。")
		return err
	}
	// This second durable boundary guarantees that the backup contains the full
	// payout intent before the request can possibly leave the process.
	if err := o.saveActivePhase(ctx, state, PhasePayoutSubmitting); err != nil {
		report.failTarget(target.Kind, "payout", "无法持久化 Payout submitting 边界，付款未提交。", "请检查 data 目录和文件系统状态。")
		return err
	}
	report.syncState(state)
	return o.submitPayoutAttempt(ctx, state, target, report, false)
}

func (o *Orchestrator) replayPreparedPayout(
	ctx context.Context,
	state *RunState,
	target *PayoutTargetState,
	report *RunReport,
) error {
	if target == nil || target.Payout == nil || target.Payout.SubmitAttempts != 1 {
		return &FatalError{Err: fmt.Errorf("exact payout replay is not eligible")}
	}
	if err := validatePayoutJournal(target); err != nil {
		return &FatalError{Err: fmt.Errorf("validate exact payout replay: %w", err)}
	}

	// Reserve the only exact replay before the HTTP request can leave this
	// process. Persisting twice advances both rotating snapshots to attempts=2,
	// so a corrupt primary cannot recover an older state and send a third time.
	target.Payout.SubmitAttempts = 2
	target.BlockedReason = ""
	for snapshot := 0; snapshot < 2; snapshot++ {
		if err := o.saveActivePhase(ctx, state, PhasePayoutReplaying); err != nil {
			report.failTarget(
				target.Kind, "payout",
				"Exact replay 边界持久化失败，未继续发送。",
				"请保留 current 状态并人工核查；禁止构造新请求。",
			)
			return &FatalError{Err: fmt.Errorf("persist exact payout replay boundary: %w", err)}
		}
	}
	report.syncState(state)
	return o.submitPayoutAttempt(ctx, state, target, report, true)
}

func (o *Orchestrator) submitPayoutAttempt(
	ctx context.Context,
	state *RunState,
	target *PayoutTargetState,
	report *RunReport,
	exactReplay bool,
) error {
	prepared := clonePrepared(target.Payout.Prepared)
	result, submitErr := o.chain.Submit(ctx, prepared)
	report.markSubmitted(target.Manifest.Settlement)

	attrs := []slog.Attr{
		slog.String("event", "funding_payout_submit_result"),
		slog.String("run_id", state.RunID),
		slog.String("payout_kind", string(target.Kind)),
		slog.String("payout_total", target.Manifest.PayoutTotal),
		slog.Int("submit_attempt", target.Payout.SubmitAttempts),
		slog.Bool("exact_replay", exactReplay),
		slog.Bool("accepted", result.Accepted),
		slog.Bool("rejected", result.Rejected),
	}
	attrs = append(attrs, slog.String("settlement_total_before", target.Payout.TotalBefore))
	if submitErr != nil {
		attrs = append(attrs, slog.String("submit_error", logging.SafeErrorText(submitErr)))
	}
	if !result.Accepted && len(result.Response) != 0 {
		attrs = append(attrs, slog.String(
			"response", logging.SafeDiagnosticExcerpt(string(result.Response), 1024),
		))
	}
	o.info(ctx, "final payout submission returned", attrs...)
	switch {
	case submitErr == nil && result.Accepted:
		return o.confirmPayout(ctx, state, target, "exchange_response", report)
	case !exactReplay && submitErr == nil && result.Rejected:
		return o.rejectPayout(ctx, state, target, report)
	default:
		return o.observeUncertainPayout(ctx, state, target, report)
	}
}

func (o *Orchestrator) observeUncertainPayout(
	ctx context.Context,
	state *RunState,
	target *PayoutTargetState,
	report *RunReport,
) error {
	markSettlementPayoutUnconfirmed(report, target.Kind)
	if err := validatePayoutJournal(target); err != nil {
		report.failTarget(target.Kind, "payout", "持久化的 Payout journal 未通过安全校验。", "请保留 current 状态，禁止直接重发。")
		return &FatalError{Err: fmt.Errorf("validate uncertain payout journal: %w", err)}
	}
	before, err := decimalFromString(target.Payout.TotalBefore)
	if err != nil {
		return err
	}
	amount, err := decimalFromString(target.Manifest.PayoutTotal)
	if err != nil {
		return err
	}
	expectedAfter, expectedAfterWithFee, feeBoundaryAvailable := payoutSettlementBoundaries(before, amount)
	if expectedAfter.IsNegative() {
		return fmt.Errorf(
			"expected post-payout settlement total is negative: before %s, payout %s",
			before, amount,
		)
	}

	successfulObservations := 0
	consecutiveConfirmations := 0
	queryErrors := 0
	allSuccessfulAtBefore := true
	lastEvidence := ""
	for queryAttempt := 1; queryAttempt <= payoutBalanceQueryAttempts &&
		successfulObservations < payoutBalanceObservationAttempts; queryAttempt++ {
		// A pre-query wait gives the balance index time to catch up and makes the
		// successful-observation count describe an actual observation window.
		if err := o.sleeper.Sleep(ctx, payoutBalanceObservationInterval); err != nil {
			return err
		}
		balance, balanceErr := o.chain.SpotBalance(ctx, target.Manifest.Settlement, *target.Manifest.Token)
		if balanceErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			queryErrors++
			consecutiveConfirmations = 0
			lastEvidence = ""
			o.warn(ctx, "uncertain payout balance query failed",
				slog.String("event", "funding_payout_balance_query_failed"),
				slog.String("run_id", state.RunID),
				slog.String("payout_kind", string(target.Kind)),
				slog.Int("query_attempt", queryAttempt),
				slog.Int("max_query_attempts", payoutBalanceQueryAttempts),
				slog.Int("successful_observations", successfulObservations),
				slog.Int("query_errors", queryErrors),
				logging.Err(balanceErr))
			continue
		}
		successfulObservations++
		if !balance.Total.Equal(before) {
			allSuccessfulAtBefore = false
		}
		evidence, matchesExpected := payoutSettlementEvidence(balance.Total, before, amount)
		if matchesExpected && evidence == lastEvidence {
			consecutiveConfirmations++
		} else if matchesExpected {
			lastEvidence = evidence
			consecutiveConfirmations = 1
		} else {
			lastEvidence = ""
			consecutiveConfirmations = 0
		}
		o.info(ctx, "uncertain payout balance observed",
			slog.String("event", "funding_payout_balance_observed"),
			slog.String("run_id", state.RunID),
			slog.String("payout_kind", string(target.Kind)),
			slog.Int("query_attempt", queryAttempt),
			slog.Int("observation", successfulObservations),
			slog.String("total_before", before.String()),
			slog.String("expected_total_after", expectedAfter.String()),
			slog.String("expected_total_after_with_activation_fee", expectedAfterWithFee.String()),
			slog.Bool("activation_fee_boundary_available", feeBoundaryAvailable),
			slog.String("total", balance.Total.String()),
			slog.String("available", balance.Available.String()),
			slog.Bool("matches_expected", matchesExpected),
			slog.String("confirmation_evidence", evidence),
			slog.Bool("all_successful_observations_at_before", allSuccessfulAtBefore),
			slog.Int("consecutive_confirmations", consecutiveConfirmations))
		if consecutiveConfirmations >= payoutBalanceRequiredConfirmations {
			return o.confirmPayout(ctx, state, target, evidence, report)
		}
	}

	allowExactReplay := (target.Phase == PhasePayoutSubmitting || target.Phase == PhasePayoutPrepared) &&
		target.Payout.SubmitAttempts == 1
	if allowExactReplay && successfulObservations == payoutBalanceObservationAttempts &&
		allSuccessfulAtBefore {
		o.info(ctx, "uncertain payout remained at pre-submit balance; exact replay authorized",
			slog.String("event", "funding_payout_exact_replay_authorized"),
			slog.String("run_id", state.RunID),
			slog.String("payout_kind", string(target.Kind)),
			slog.Uint64("nonce", target.Payout.Prepared.Nonce))
		return o.replayPreparedPayout(ctx, state, target, report)
	}
	reason := fmt.Sprintf(
		"%s after %d successful settlement balance observations, %d query errors, and %d submit attempts",
		uncertainPayoutBlockedReasonPrefix, successfulObservations, queryErrors,
		target.Payout.SubmitAttempts,
	)
	return o.blockUncertainPayout(ctx, state, target, report,
		reason,
		"payout_ambiguous")
}

func payoutSettlementBoundaries(
	before, amount decimal.Decimal,
) (withoutFee, withFee decimal.Decimal, feeBoundaryAvailable bool) {
	withoutFee = before.Sub(amount)
	withFee = withoutFee.Sub(payoutNewAddressActivationFee)
	return withoutFee, withFee, !withFee.IsNegative()
}

func payoutSettlementEvidence(total, before, amount decimal.Decimal) (string, bool) {
	withoutFee, withFee, feeBoundaryAvailable := payoutSettlementBoundaries(before, amount)
	if !withoutFee.IsNegative() && total.Equal(withoutFee) {
		return "settlement_balance_decreased", true
	}
	if feeBoundaryAvailable && total.Equal(withFee) {
		return "settlement_balance_decreased_with_activation_fee", true
	}
	return "", false
}

func (o *Orchestrator) confirmPayout(
	ctx context.Context,
	state *RunState,
	target *PayoutTargetState,
	evidence string,
	report *RunReport,
) error {
	target.BlockedReason = ""
	target.ConfirmationEvidence = evidence
	if err := o.saveActivePhase(ctx, state, PhasePayoutConfirmed); err != nil {
		report.failTarget(target.Kind, "payout", "Payout 已确认，但确认状态持久化失败。", "请保留现场并核查链上付款，禁止直接重发。")
		report.target(target.Kind).Payout.Status = ReportStepSuccess
		return err
	}
	report.target(target.Kind).Payout.Status = ReportStepSuccess
	report.target(target.Kind).Payout.ConfirmationEvidence = evidence
	if report.SettlementBalance.PostPayoutUnconfirmed {
		report.SettlementBalance = SettlementBalanceReport{Address: target.Manifest.Settlement}
	}
	report.setTargetStage(target.Kind, "payout", ReportStepSuccess, payoutEvidenceSummary(evidence))
	report.syncState(state)
	attrs := []slog.Attr{
		slog.String("event", "funding_payout_confirmed"),
		slog.String("run_id", state.RunID), slog.String("evidence", evidence),
		slog.String("payout_kind", string(target.Kind)),
		slog.String("payout_total", target.Manifest.PayoutTotal),
	}
	if target.Payout != nil {
		attrs = append(attrs, slog.String("settlement_total_before", target.Payout.TotalBefore))
	}
	o.info(ctx, "final payout confirmed", attrs...)
	return o.completeDatabase(ctx, state, target, report)
}

func (o *Orchestrator) rejectPayout(
	ctx context.Context,
	state *RunState,
	target *PayoutTargetState,
	report *RunReport,
) error {
	const reason = explicitlyRejectedPayoutBlockedReason
	report.target(target.Kind).Payout.Status = ReportStepFailed
	report.Outcome = "rejected"
	report.failTarget(
		target.Kind, "payout",
		"Payout 被 Hyperliquid 明确拒绝，已确认没有发生付款。",
		"修复拒绝原因后可以重启服务；数据库记录仍保持 pending。",
	)
	target.BlockedReason = reason
	for snapshot := 0; snapshot < 2; snapshot++ {
		if err := o.saveActivePhase(ctx, state, PhaseBlocked); err != nil {
			report.failTarget(target.Kind, "payout", "Payout 被明确拒绝，拒绝状态持久化失败。", "请保留现场并检查 data 目录。")
			return &FatalError{Err: fmt.Errorf("persist rejected payout state: %w", err)}
		}
	}
	report.syncState(state)
	if err := o.store.Archive(ctx, *state, "rejected"); err != nil {
		report.fail("finalize", "明确拒绝现场归档失败。", "请检查 data/history 目录并保留 current 状态。")
		return &FatalError{Err: fmt.Errorf("archive rejected payout: %w", err)}
	}
	// An explicit rejection proves that no payout occurred. Clearing current is
	// therefore safe; after the fatal process exit an operator may restart and
	// retry the still-pending database records.
	if err := o.store.Clear(ctx); err != nil {
		report.fail("finalize", "明确拒绝现场已归档，但 current 状态清理失败。", "请检查 data 目录并保留现场。")
		return &FatalError{Err: fmt.Errorf("clear rejected payout state: %w", err)}
	}
	report.setStage("finalize", ReportStepSuccess, "明确拒绝现场已归档并清理")
	o.error(ctx, "final payout rejected",
		slog.String("event", "funding_payout_rejected"), slog.String("run_id", state.RunID))
	o.alertOnce(ctx, state.RunID, "payout_rejected", AlertSeverityCritical, reason)
	return &FatalError{Err: fmt.Errorf("%s", reason)}
}

func (o *Orchestrator) blockUncertainPayout(
	ctx context.Context,
	state *RunState,
	target *PayoutTargetState,
	report *RunReport,
	reason, alertKey string,
) error {
	report.target(target.Kind).Payout.Status = ReportStepFailed
	report.Outcome = "blocked"
	report.failTarget(
		target.Kind, "payout",
		"Payout 结果仍不确定，Settlement total 未能稳定达到任一精确付款后余额。",
		"已停止观察和重试；请结合最终报告与持久化的完整请求，核查 nonce 与 Settlement 余额。",
	)
	target.BlockedReason = reason
	// Make terminality survive primary corruption: both rotating snapshots must
	// say blocked before the run is reported as finished.
	for snapshot := 0; snapshot < 2; snapshot++ {
		if err := o.saveActivePhase(ctx, state, PhaseBlocked); err != nil {
			report.failTarget(target.Kind, "payout", "Payout 结果不确定，且 blocked 状态持久化失败。", "立即停止服务并保留现场，禁止重发。")
			return &FatalError{Err: fmt.Errorf("persist blocked payout state: %w", err)}
		}
	}
	report.syncState(state)
	if err := o.store.Archive(ctx, *state, "blocked"); err != nil {
		report.fail("finalize", "Blocked Payout 现场归档失败。", "请检查 data/history 目录并保留 current 状态。")
		return &FatalError{Err: fmt.Errorf("archive blocked payout: %w", err)}
	}
	report.setStage("finalize", ReportStepSuccess, "Blocked 现场已归档，current 已保留")
	o.error(ctx, "final payout blocked",
		slog.String("event", "funding_payout_blocked"),
		slog.String("run_id", state.RunID), slog.String("reason", reason))
	o.alertOnce(ctx, state.RunID, alertKey, AlertSeverityCritical, reason)
	return &FatalError{Err: fmt.Errorf("%s", reason)}
}

func isUncertainPayoutBlock(target *PayoutTargetState) bool {
	return target != nil && target.Payout != nil &&
		strings.HasPrefix(target.BlockedReason, uncertainPayoutBlockedReasonPrefix)
}

func payoutEvidenceSummary(evidence string) string {
	switch evidence {
	case "exchange_response":
		return "Exchange 明确确认"
	case "settlement_balance_decreased":
		return "Settlement 达到预期付款后余额"
	case "settlement_balance_decreased_with_activation_fee":
		return "Settlement 达到含 1 USDC 新地址费用的预期余额"
	default:
		return "Payout 已确认"
	}
}
