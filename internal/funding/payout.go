package funding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"builder-code-bot/internal/hyperliquid/info"

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
	before info.SpotBalanceAmounts,
	amount decimal.Decimal,
	report *RunReport,
) error {
	recipientBefore, err := o.payoutRecipientBalanceBaseline(ctx, state, report)
	if err != nil {
		return err
	}
	report.Payout.Amount = amount.String()
	report.Payout.SettlementTotalBefore = before.Total.String()
	o.info(ctx, "final payout preparation started",
		slog.String("event", "funding_payout_preparation_started"),
		slog.String("run_id", state.RunID), slog.String("payout_total", amount.String()),
		slog.String("settlement_total_before", before.Total.String()),
		slog.String("recipient_total_before", recipientBefore.Total.String()),
		slog.String("recipient", state.Manifest.Recipient))
	prepared, err := o.chain.PrepareSpotSend(
		state.Manifest.Settlement, state.Manifest.Recipient, *state.Manifest.Token, amount, o.nonce.Next(),
	)
	if err != nil {
		o.alertOnce(
			ctx, state.RunID, "payout_prepare_failed",
			AlertSeverityWarning, "final payout preparation failed",
		)
		report.fail("payout", "Payout 请求准备失败。", "请检查 Settlement signer、Token 和收款地址配置。")
		return err
	}
	if err := validatePreparedPayout(state, prepared); err != nil {
		report.fail("payout", "Payout 请求未通过安全校验。", "请保留现场并检查不可变 Manifest 和签名请求。")
		return err
	}
	report.Payout.RequestHash = prepared.RequestHash
	report.Payout.Nonce = prepared.Nonce
	state.Payout = &PayoutJournal{
		Prepared: clonePrepared(prepared), TotalBefore: before.Total.String(),
		RecipientTotalBefore: recipientBefore.Total.String(),
	}
	if err := o.save(ctx, state, PhasePayoutPrepared); err != nil {
		report.fail("payout", "Payout 意图持久化失败，付款未开始。", "请检查 data 目录和文件系统状态。")
		return err
	}
	report.syncState(state)
	return o.submitPreparedPayout(ctx, state, report)
}

func (o *Orchestrator) payoutRecipientBalanceBaseline(
	ctx context.Context,
	state *RunState,
	report *RunReport,
) (info.SpotBalanceAmounts, error) {
	var (
		candidate   info.SpotBalanceAmounts
		lastErr     error
		consecutive int
	)
	for attempt := 1; attempt <= settlementBalanceQueryAttempts; attempt++ {
		balance, err := o.chain.SpotBalance(ctx, state.Manifest.Recipient, *state.Manifest.Token)
		if err == nil {
			lastErr = nil
			if consecutive > 0 && balance.Total.Equal(candidate.Total) {
				consecutive++
			} else {
				candidate = balance
				consecutive = 1
			}
			o.info(ctx, "payout recipient balance baseline observed",
				slog.String("event", "funding_payout_recipient_balance_baseline_observed"),
				slog.String("run_id", state.RunID),
				slog.String("recipient", state.Manifest.Recipient),
				slog.String("total", balance.Total.String()),
				slog.String("available", balance.Available.String()),
				slog.Int("attempt", attempt),
				slog.Int("consecutive_stable_reads", consecutive))
			if consecutive >= settlementBalanceRequiredConfirmations {
				return balance, nil
			}
		} else {
			lastErr = err
			consecutive = 0
		}
		if attempt < settlementBalanceQueryAttempts {
			if sleepErr := o.sleeper.Sleep(ctx, settlementBalanceQueryInterval); sleepErr != nil {
				return info.SpotBalanceAmounts{}, sleepErr
			}
		}
	}
	report.fail(
		"payout",
		"收款方余额基线未能稳定，无法建立 Payout 不确定结果的双边确认边界。",
		"请检查 Hyperliquid Info API；在取得收款方余额基线前不会提交 Payout。",
	)
	if lastErr == nil {
		lastErr = fmt.Errorf("payout recipient balance did not stabilize")
	}
	return info.SpotBalanceAmounts{}, fmt.Errorf("query payout recipient balance baseline: %w", lastErr)
}

func (o *Orchestrator) ensurePreparedPayoutRecipientBoundary(
	ctx context.Context,
	state *RunState,
	report *RunReport,
) error {
	if err := validatePayoutJournal(state, false); err != nil {
		report.fail("payout", "持久化的 Payout journal 未通过安全校验。", "请保留 current 状态，禁止直接重发。")
		return err
	}
	if strings.TrimSpace(state.Payout.RecipientTotalBefore) != "" {
		return nil
	}
	// A primary payout_prepared snapshot proves Submit has not started, so a
	// legacy journal can safely acquire the recipient boundary before sending.
	// Backup/submitting snapshots must never call this helper.
	recipientBefore, err := o.payoutRecipientBalanceBaseline(ctx, state, report)
	if err != nil {
		return err
	}
	state.Payout.RecipientTotalBefore = recipientBefore.Total.String()
	if err := o.save(ctx, state, PhasePayoutPrepared); err != nil {
		report.fail("payout", "收款方余额基线持久化失败，付款未提交。", "请检查 data 目录和文件系统状态。")
		return err
	}
	report.syncState(state)
	return nil
}

func (o *Orchestrator) submitPreparedPayout(ctx context.Context, state *RunState, report *RunReport) error {
	if err := validatePayoutJournal(state, true); err != nil {
		report.fail("payout", "持久化的 Payout journal 未通过安全校验。", "请保留 current 状态，禁止直接重发。")
		return err
	}
	// This second durable boundary guarantees that the backup contains the full
	// payout intent before the request can possibly leave the process.
	if err := o.save(ctx, state, PhasePayoutSubmitting); err != nil {
		report.fail("payout", "无法持久化 Payout submitting 边界，付款未提交。", "请检查 data 目录和文件系统状态。")
		return err
	}
	report.syncState(state)

	result, _ := o.chain.Submit(ctx, clonePrepared(state.Payout.Prepared))
	report.markSubmitted(state.Manifest.Settlement)
	attrs := []slog.Attr{
		slog.String("event", "funding_payout_submit_result"),
		slog.String("run_id", state.RunID),
		slog.String("payout_total", state.Manifest.PayoutTotal),
		slog.Bool("accepted", result.Accepted),
		slog.Bool("rejected", result.Rejected),
	}
	if state.Payout != nil {
		attrs = append(attrs, slog.String("settlement_total_before", state.Payout.TotalBefore))
	}
	if !result.Accepted && len(result.Response) != 0 {
		attrs = append(attrs, slog.String("response", string(result.Response)))
	}
	o.info(ctx, "final payout submission returned", attrs...)
	switch {
	case result.Accepted:
		return o.confirmPayout(ctx, state, "exchange_response", report)
	case result.Rejected:
		return o.rejectPayout(ctx, state, report)
	default:
		return o.observeUncertainPayout(ctx, state, report)
	}
}

func (o *Orchestrator) observeUncertainPayout(ctx context.Context, state *RunState, report *RunReport) error {
	markSettlementPayoutUnconfirmed(report)
	if err := validatePayoutJournal(state, false); err != nil {
		report.fail("payout", "持久化的 Payout journal 未通过安全校验。", "请保留 current 状态，禁止直接重发。")
		return &FatalError{Err: fmt.Errorf("validate uncertain payout journal: %w", err)}
	}
	before, err := decimalFromString(state.Payout.TotalBefore)
	if err != nil {
		return err
	}
	amount, err := decimalFromString(state.Manifest.PayoutTotal)
	if err != nil {
		return err
	}
	expectedAfter := before.Sub(amount)
	if expectedAfter.IsNegative() {
		return fmt.Errorf(
			"expected post-payout settlement total is negative: before %s, payout %s",
			before, amount,
		)
	}
	recipientBefore, recipientBeforeErr := decimalFromString(state.Payout.RecipientTotalBefore)
	recipientBoundaryAvailable := recipientBeforeErr == nil
	expectedRecipient := recipientBefore.Add(amount)

	successfulObservations := 0
	consecutiveConfirmations := 0
	queryErrors := 0
	preBalanceObserved := state.Payout.PreBalanceObservedAfterSubmit
	for queryAttempt := 1; queryAttempt <= payoutBalanceQueryAttempts &&
		successfulObservations < payoutBalanceObservationAttempts; queryAttempt++ {
		// A pre-query wait gives the balance index time to catch up and makes the
		// successful-observation count describe an actual observation window.
		if err := o.sleeper.Sleep(ctx, payoutBalanceObservationInterval); err != nil {
			return err
		}
		balance, balanceErr := o.chain.SpotBalance(ctx, state.Manifest.Settlement, *state.Manifest.Token)
		if balanceErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			queryErrors++
			consecutiveConfirmations = 0
			o.warn(ctx, "uncertain payout balance query failed",
				slog.String("event", "funding_payout_balance_query_failed"),
				slog.String("run_id", state.RunID),
				slog.Int("query_attempt", queryAttempt),
				slog.Int("max_query_attempts", payoutBalanceQueryAttempts),
				slog.Int("successful_observations", successfulObservations),
				slog.Int("query_errors", queryErrors))
			continue
		}
		if balance.Total.Equal(before) && !preBalanceObserved {
			state.Payout.PreBalanceObservedAfterSubmit = true
			state.BlockedReason = ""
			// Save twice so both the primary and rotating backup carry this
			// post-submit evidence. A crash plus a corrupt primary must not fall
			// back to a snapshot that can no longer auto-confirm a visible payout.
			for copyIndex := 0; copyIndex < 2; copyIndex++ {
				if err := o.save(ctx, state, PhasePayoutSubmitting); err != nil {
					report.fail(
						"payout",
						"Payout 前态证据持久化失败，已停止自动确认。",
						"请保留 current 状态并人工核查；禁止重发。",
					)
					return &FatalError{Err: fmt.Errorf("persist payout pre-balance evidence: %w", err)}
				}
			}
			report.syncState(state)
			preBalanceObserved = true
		}
		recipientBalance, recipientErr := o.chain.SpotBalance(
			ctx, state.Manifest.Recipient, *state.Manifest.Token,
		)
		if recipientErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			queryErrors++
			consecutiveConfirmations = 0
			o.warn(ctx, "uncertain payout recipient balance query failed",
				slog.String("event", "funding_payout_recipient_balance_query_failed"),
				slog.String("run_id", state.RunID),
				slog.Int("query_attempt", queryAttempt),
				slog.Int("max_query_attempts", payoutBalanceQueryAttempts),
				slog.Int("successful_observations", successfulObservations),
				slog.Int("query_errors", queryErrors))
			continue
		}

		successfulObservations++
		matchesSettlement := balance.Total.Equal(expectedAfter)
		matchesRecipient := recipientBoundaryAvailable &&
			recipientBalance.Total.Equal(expectedRecipient)
		matchesExpected := matchesSettlement && matchesRecipient
		if matchesExpected && preBalanceObserved {
			consecutiveConfirmations++
		} else {
			consecutiveConfirmations = 0
		}
		o.info(ctx, "uncertain payout balance observed",
			slog.String("event", "funding_payout_balance_observed"),
			slog.String("run_id", state.RunID),
			slog.Int("query_attempt", queryAttempt),
			slog.Int("observation", successfulObservations),
			slog.String("total_before", before.String()),
			slog.String("expected_total_after", expectedAfter.String()),
			slog.String("total", balance.Total.String()),
			slog.String("available", balance.Available.String()),
			slog.String("expected_recipient_total_after", expectedRecipient.String()),
			slog.String("recipient_total", recipientBalance.Total.String()),
			slog.Bool("recipient_boundary_available", recipientBoundaryAvailable),
			slog.Bool("matches_settlement", matchesSettlement),
			slog.Bool("matches_recipient", matchesRecipient),
			slog.Bool("matches_expected", matchesExpected),
			slog.Bool("pre_balance_observed_after_submit", preBalanceObserved),
			slog.Int("consecutive_confirmations", consecutiveConfirmations))
		if consecutiveConfirmations >= payoutBalanceRequiredConfirmations {
			return o.confirmPayout(ctx, state, "paired_balance_boundary", report)
		}
	}
	reason := fmt.Sprintf(
		"%s after %d successful settlement balance observations and %d query errors",
		uncertainPayoutBlockedReasonPrefix, successfulObservations, queryErrors,
	)
	return o.blockUncertainPayout(ctx, state, report,
		reason,
		"payout_ambiguous")
}

func (o *Orchestrator) confirmPayout(
	ctx context.Context,
	state *RunState,
	evidence string,
	report *RunReport,
) error {
	state.BlockedReason = ""
	if err := o.save(ctx, state, PhasePayoutConfirmed); err != nil {
		report.fail("payout", "Payout 已确认，但确认状态持久化失败。", "请保留现场并核查链上付款，禁止直接重发。")
		report.Payout.Status = ReportStepSuccess
		return err
	}
	report.Payout.Status = ReportStepSuccess
	report.Payout.ConfirmationEvidence = evidence
	report.setStage("payout", ReportStepSuccess, payoutEvidenceSummary(evidence))
	report.syncState(state)
	attrs := []slog.Attr{
		slog.String("event", "funding_payout_confirmed"),
		slog.String("run_id", state.RunID), slog.String("evidence", evidence),
		slog.String("payout_total", state.Manifest.PayoutTotal),
	}
	if state.Payout != nil {
		attrs = append(attrs, slog.String("settlement_total_before", state.Payout.TotalBefore))
	}
	o.info(ctx, "final payout confirmed", attrs...)
	return o.completeDatabase(ctx, state, report)
}

func (o *Orchestrator) rejectPayout(ctx context.Context, state *RunState, report *RunReport) error {
	const reason = explicitlyRejectedPayoutBlockedReason
	report.Payout.Status = ReportStepFailed
	report.Outcome = "rejected"
	report.fail(
		"payout",
		"Payout 被 Hyperliquid 明确拒绝，已确认没有发生付款。",
		"修复拒绝原因后可以重启服务；数据库记录仍保持 pending。",
	)
	state.BlockedReason = reason
	if err := o.save(ctx, state, PhaseBlocked); err != nil {
		report.fail("payout", "Payout 被明确拒绝，拒绝状态持久化失败。", "请保留现场并检查 data 目录。")
		return &FatalError{Err: fmt.Errorf("persist rejected payout state: %w", err)}
	}
	report.syncState(state)
	if err := o.store.Archive(ctx, *state, "rejected"); err != nil {
		return &FatalError{Err: fmt.Errorf("archive rejected payout: %w", err)}
	}
	// An explicit rejection proves that no payout occurred. Clearing current is
	// therefore safe; after the fatal process exit an operator may restart and
	// retry the still-pending database records.
	if err := o.store.Clear(ctx); err != nil {
		return &FatalError{Err: fmt.Errorf("clear rejected payout state: %w", err)}
	}
	o.error(ctx, "final payout rejected",
		slog.String("event", "funding_payout_rejected"), slog.String("run_id", state.RunID))
	o.alertOnce(ctx, state.RunID, "payout_rejected", AlertSeverityCritical, reason)
	return &FatalError{Err: fmt.Errorf("%s", reason)}
}

func (o *Orchestrator) blockUncertainPayout(
	ctx context.Context,
	state *RunState,
	report *RunReport,
	reason, alertKey string,
) error {
	report.Payout.Status = ReportStepFailed
	report.Outcome = "blocked"
	report.fail(
		"payout",
		"Payout 结果仍不确定，Settlement total 未能稳定达到预期付款后余额。",
		"需要人工核查收款方、request hash、nonce 和 Settlement 余额；在确认前禁止重发。",
	)
	state.BlockedReason = reason
	if err := o.save(ctx, state, PhaseBlocked); err != nil {
		report.fail("payout", "Payout 结果不确定，且 blocked 状态持久化失败。", "立即停止服务并保留现场，禁止重发。")
		return &FatalError{Err: fmt.Errorf("persist blocked payout state: %w", err)}
	}
	report.syncState(state)
	if err := o.store.Archive(ctx, *state, "blocked"); err != nil {
		return &FatalError{Err: fmt.Errorf("archive blocked payout: %w", err)}
	}
	o.error(ctx, "final payout blocked",
		slog.String("event", "funding_payout_blocked"),
		slog.String("run_id", state.RunID), slog.String("reason", reason))
	o.alertOnce(ctx, state.RunID, alertKey, AlertSeverityCritical, reason)
	return &FatalError{Err: fmt.Errorf("%s", reason)}
}

func isUncertainPayoutBlock(state *RunState) bool {
	return state != nil && state.Payout != nil &&
		strings.HasPrefix(state.BlockedReason, uncertainPayoutBlockedReasonPrefix)
}

func payoutEvidenceSummary(evidence string) string {
	switch evidence {
	case "exchange_response":
		return "Exchange 明确确认"
	case "settlement_balance_decreased":
		return "Settlement 达到预期付款后余额"
	case "paired_balance_boundary":
		return "Settlement 与 Recipient 达到预期付款后余额"
	default:
		return "Payout 已确认"
	}
}
