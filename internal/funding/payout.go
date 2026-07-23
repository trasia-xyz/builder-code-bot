package funding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/shopspring/decimal"
)

const (
	payoutBalanceObservationAttempts = 5
	payoutBalanceObservationInterval = time.Second
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
	before, amount decimal.Decimal,
	report *RunReport,
) error {
	report.Payout.Amount = amount.String()
	report.Payout.SettlementTotalBefore = before.String()
	o.info(ctx, "final payout preparation started",
		slog.String("event", "funding_payout_preparation_started"),
		slog.String("run_id", state.RunID), slog.String("payout_total", amount.String()),
		slog.String("settlement_total_before", before.String()),
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
	state.Payout = &PayoutJournal{Prepared: clonePrepared(prepared), TotalBefore: before.String()}
	if err := o.save(ctx, state, PhasePayoutPrepared); err != nil {
		report.fail("payout", "Payout 意图持久化失败，付款未开始。", "请检查 data 目录和文件系统状态。")
		return err
	}
	report.syncState(state)
	return o.submitPreparedPayout(ctx, state, report)
}

func (o *Orchestrator) submitPreparedPayout(ctx context.Context, state *RunState, report *RunReport) error {
	if state.Payout == nil {
		report.fail("payout", "持久化的 Payout journal 缺失。", "请保留 current 状态，禁止直接重发。")
		return fmt.Errorf("persisted payout journal is missing")
	}
	if err := validatePreparedPayout(state, state.Payout.Prepared); err != nil {
		report.fail("payout", "持久化的 Payout 请求未通过安全校验。", "请保留 current 状态，禁止直接重发。")
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
	if len(result.Response) != 0 {
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
	if state.Payout == nil {
		report.fail("payout", "持久化的 Payout journal 缺失。", "请保留 current 状态，禁止直接重发。")
		return fmt.Errorf("persisted payout journal is missing")
	}
	if err := validatePreparedPayout(state, state.Payout.Prepared); err != nil {
		report.fail("payout", "持久化的 Payout 请求未通过安全校验。", "请保留 current 状态，禁止直接重发。")
		return err
	}
	before, err := decimalFromString(state.Payout.TotalBefore)
	if err != nil {
		return err
	}
	for attempt := 1; attempt <= payoutBalanceObservationAttempts; attempt++ {
		balance, balanceErr := o.chain.SpotBalance(ctx, state.Manifest.Settlement, *state.Manifest.Token)
		if balanceErr != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if balanceErr == nil {
			decreased := balance.Total.LessThan(before)
			o.info(ctx, "uncertain payout balance observed",
				slog.String("event", "funding_payout_balance_observed"),
				slog.String("run_id", state.RunID), slog.Int("attempt", attempt),
				slog.String("total_before", before.String()),
				slog.String("total", balance.Total.String()),
				slog.String("available", balance.Available.String()),
				slog.Bool("decreased", decreased))
			if decreased {
				return o.confirmPayout(ctx, state, "settlement_balance_decreased", report)
			}
		}
		if attempt < payoutBalanceObservationAttempts {
			if err := o.sleeper.Sleep(ctx, payoutBalanceObservationInterval); err != nil {
				return err
			}
		}
	}
	return o.blockUncertainPayout(ctx, state, report,
		"final payout result remained uncertain and settlement balance did not decrease",
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
	const reason = "final payout was explicitly rejected"
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
		"Payout 结果仍不确定，Settlement total 未观察到下降。",
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

func payoutEvidenceSummary(evidence string) string {
	switch evidence {
	case "exchange_response":
		return "Exchange 明确确认"
	case "settlement_balance_decreased":
		return "Settlement total 下降确认"
	default:
		return "Payout 已确认"
	}
}
