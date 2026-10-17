package funding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"builder-code-bot/internal/logging"
)

const fundingTaskConsolePrefix = "=========="

const reportDeliveryTimeout = 15 * time.Second

func (o *Orchestrator) Run(ctx context.Context, trigger Trigger) error {
	return o.run(ctx, trigger, ReportExecution{})
}

func (o *Orchestrator) RunScheduled(
	ctx context.Context,
	trigger Trigger,
	attempt, maxAttempts int,
) error {
	return o.run(ctx, trigger, ReportExecution{Attempt: attempt, MaxAttempts: maxAttempts})
}

func (o *Orchestrator) run(ctx context.Context, trigger Trigger, execution ReportExecution) (err error) {
	report := newRunReport(
		o.clock.Now(), trigger, execution, o.network, o.builders, o.builderNames,
		o.settlement, o.recipient,
	)
	defer func() { o.deliverRunReport(ctx, &report, err) }()
	defer func() {
		if !errors.Is(err, context.Canceled) {
			o.observeUserRateLimits(rateLimitObservationContext(ctx), &report)
		}
	}()
	defer func() {
		if !errors.Is(err, context.Canceled) {
			o.observeSettlementFinalBalance(rateLimitObservationContext(ctx), &report)
		}
	}()
	o.info(ctx, "funding task started",
		logging.ConsoleSeparator(),
		logging.ConsolePrefix(fundingTaskConsolePrefix),
		slog.String("event", "funding_task_started"),
		slog.String("trigger", string(trigger)))
	current, metadata, err := o.loadCurrent(ctx)
	consolidationDate := report.StartedAt.Format(time.DateOnly)
	if current != nil && current.UTCDate != "" {
		consolidationDate = current.UTCDate
	}
	o.attachDailyConsolidation(ctx, &report, consolidationDate)
	if err != nil {
		return err
	}
	if current != nil {
		report.Recovery = true
		report.Outcome = "completed"
		report.syncState(current)
		report.setStage("snapshot", ReportStepSuccess, "从持久化状态恢复")
		return o.recoverState(ctx, current, metadata, &report)
	}
	return o.runNew(ctx, trigger, &report)
}

func (o *Orchestrator) Recover(ctx context.Context) (err error) {
	o.info(ctx, "funding recovery check started",
		logging.ConsoleSeparator(),
		logging.ConsolePrefix(fundingTaskConsolePrefix),
		slog.String("event", "funding_recovery_check_started"))
	current, metadata, err := o.loadCurrent(ctx)
	if err != nil || current == nil {
		return err
	}
	report := newRunReport(
		o.clock.Now(), current.Trigger, ReportExecution{Recovery: true}, o.network,
		o.builders, o.builderNames, o.settlement,
		o.recipient,
	)
	report.Outcome = "completed"
	report.syncState(current)
	report.setStage("snapshot", ReportStepSuccess, "从持久化状态恢复")
	o.attachDailyConsolidation(ctx, &report, current.UTCDate)
	defer func() { o.deliverRunReport(ctx, &report, err) }()
	defer func() {
		if !errors.Is(err, context.Canceled) {
			o.observeUserRateLimits(rateLimitObservationContext(ctx), &report)
		}
	}()
	defer func() {
		if !errors.Is(err, context.Canceled) {
			o.observeSettlementFinalBalance(rateLimitObservationContext(ctx), &report)
		}
	}()
	return o.recoverState(ctx, current, metadata, &report)
}

func (o *Orchestrator) recoverState(
	ctx context.Context,
	state *RunState,
	metadata StateLoadMetadata,
	report *RunReport,
) (err error) {
	target, _, targetErr := state.activePayoutTarget()
	if targetErr != nil {
		return &FatalError{Err: targetErr}
	}
	if target == nil {
		return o.finishRun(ctx, state, report)
	}
	if err := validateTargetProgress(state, target); err != nil {
		return &FatalError{Err: err}
	}
	markRecoveredPayoutSubmission(report, state, target, metadata)
	o.info(ctx, "funding recovery started",
		slog.String("event", "recovery_started"), slog.String("run_id", state.RunID),
		slog.String("payout_kind", string(target.Kind)),
		slog.String("phase", string(target.Phase)),
		slog.Int("record_count", len(target.Manifest.Records)),
		slog.String("raw_total", target.Manifest.RawTotal),
		slog.String("payout_total", target.Manifest.PayoutTotal))
	defer func() {
		if err == nil {
			o.info(ctx, "funding recovery completed",
				slog.String("event", "recovery_completed"), slog.String("run_id", state.RunID))
		}
	}()
	switch target.Phase {
	case PhasePrepared:
		if len(target.Manifest.Records) == 0 {
			report.markTargetNoData(target.Kind)
			return o.completeDatabase(ctx, state, target, report)
		}
		if target.Manifest.PayoutTotal == "0" {
			report.setTargetStage(target.Kind, "payout", ReportStepSkipped, "无需付款")
			report.target(target.Kind).Payout.Status = ReportStepSkipped
			return o.completeDatabase(ctx, state, target, report)
		}
		return o.runPositive(ctx, state, target, report)
	case PhasePayoutPrepared:
		// A primary payout_prepared state proves that submission never started.
		// A backup at the same phase may be one boundary behind a submitted
		// primary, so it must use balance observation instead of resubmission.
		if metadata.RecoveredFromBackup {
			return o.observeUncertainPayout(ctx, state, target, report)
		}
		return o.submitPreparedPayout(ctx, state, target, report)
	case PhasePayoutSubmitting:
		return o.observeUncertainPayout(ctx, state, target, report)
	case PhasePayoutReplaying:
		// The sole replay was reserved durably before its HTTP call. Recovery
		// can only observe; it must never create a third Submit attempt.
		return o.observeUncertainPayout(ctx, state, target, report)
	case PhasePayoutConfirmed:
		if err := validateConfirmedPayoutState(target); err != nil {
			markSettlementPayoutUnconfirmed(report, target.Kind)
			report.target(target.Kind).Payout.Status = ReportStepFailed
			report.Outcome = "blocked"
			report.failTarget(
				target.Kind, "payout",
				"已确认 Payout 状态不完整，未更新数据库。",
				"请保留 current 状态并核查 journal、request hash 与付款前余额边界。",
			)
			return &FatalError{Err: fmt.Errorf("validate confirmed payout state: %w", err)}
		}
		report.target(target.Kind).Payout.Status = ReportStepSuccess
		report.setTargetStage(target.Kind, "payout", ReportStepSuccess, "已在此前执行中确认")
		return o.completeDatabase(ctx, state, target, report)
	case PhaseBlocked:
		if target.BlockedReason == "" {
			target.BlockedReason = "funding run is blocked"
		}
		markSettlementPayoutUnconfirmed(report, target.Kind)
		report.target(target.Kind).Payout.Status = ReportStepFailed
		report.Outcome = "blocked"
		report.failTarget(
			target.Kind, "payout",
			"检测到终态 blocked Payout，任务不会继续观察或重发。",
			"请根据最终报告人工核查；程序不会为该 journal 构造新请求。",
		)
		return &FatalError{Err: fmt.Errorf("%s", target.BlockedReason)}
	default:
		return fmt.Errorf("unsupported persisted funding phase %q for %s", target.Phase, target.Kind)
	}
}

func markRecoveredPayoutSubmission(
	report *RunReport,
	state *RunState,
	target *PayoutTargetState,
	metadata StateLoadMetadata,
) {
	if report == nil || state == nil || target == nil || target.Payout == nil {
		return
	}
	possiblySubmitted := false
	switch target.Phase {
	case PhasePayoutSubmitting, PhasePayoutReplaying, PhasePayoutConfirmed, PhaseBlocked:
		possiblySubmitted = true
	case PhasePayoutPrepared:
		// A backup can be one durable boundary behind a primary from which
		// Submit already started. A primary prepared state itself is safe.
		possiblySubmitted = metadata.RecoveredFromBackup
	}
	if possiblySubmitted {
		report.markSubmitted(target.Manifest.Settlement)
	}
}

func (o *Orchestrator) deliverRunReport(ctx context.Context, report *RunReport, runErr error) {
	if o.notifier == nil || report == nil || !report.finalize(o.clock.Now(), runErr) {
		return
	}
	deliveryCtx, cancel := context.WithTimeout(rateLimitObservationContext(ctx), reportDeliveryTimeout)
	defer cancel()
	o.notifier.Report(deliveryCtx, *report)
}
