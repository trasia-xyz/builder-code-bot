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
		o.builders, o.builderNames, o.settlement, o.recipient,
	)
	report.Outcome = "completed"
	report.syncState(current)
	report.setStage("snapshot", ReportStepSuccess, "从持久化状态恢复")
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
	o.info(ctx, "funding recovery started",
		slog.String("event", "recovery_started"), slog.String("run_id", state.RunID),
		slog.String("phase", string(state.Phase)),
		slog.Int("record_count", len(state.Manifest.Records)),
		slog.String("raw_total", state.Manifest.RawTotal),
		slog.String("payout_total", state.Manifest.PayoutTotal))
	defer func() {
		if err == nil {
			o.info(ctx, "funding recovery completed",
				slog.String("event", "recovery_completed"), slog.String("run_id", state.RunID))
		}
	}()
	switch state.Phase {
	case PhasePrepared:
		if state.Manifest.PayoutTotal == "0" {
			report.setStage("rewards", ReportStepSkipped, "Payout 为 0")
			report.setStage("sweep", ReportStepSkipped, "Payout 为 0")
			report.setStage("payout", ReportStepSkipped, "无需付款")
			report.Payout.Status = ReportStepSkipped
			return o.completeDatabase(ctx, state, report)
		}
		return o.runPositive(ctx, state, report)
	case PhasePayoutPrepared:
		markRecoveredBuilderSteps(report)
		// A primary payout_prepared state proves that submission never started.
		// A backup at the same phase may be one boundary behind a submitted
		// primary, so it must use balance observation instead of resubmission.
		if metadata.RecoveredFromBackup {
			return o.observeUncertainPayout(ctx, state, report)
		}
		return o.submitPreparedPayout(ctx, state, report)
	case PhasePayoutSubmitting:
		markRecoveredBuilderSteps(report)
		return o.observeUncertainPayout(ctx, state, report)
	case PhasePayoutConfirmed:
		markRecoveredBuilderSteps(report)
		report.Payout.Status = ReportStepSuccess
		report.setStage("payout", ReportStepSuccess, "已在此前执行中确认")
		return o.completeDatabase(ctx, state, report)
	case PhaseBlocked:
		if state.BlockedReason == "" {
			state.BlockedReason = "funding run is blocked"
		}
		report.Payout.Status = ReportStepFailed
		report.Outcome = "blocked"
		report.fail(
			"payout",
			"检测到 blocked Payout 状态。",
			"需要人工核查付款结果；在确认前禁止清理状态或重发。",
		)
		return &FatalError{Err: fmt.Errorf("%s", state.BlockedReason)}
	default:
		return fmt.Errorf("unsupported persisted funding phase %q", state.Phase)
	}
}

func markRecoveredBuilderSteps(report *RunReport) {
	const summary = "历史明细未持久化，本次恢复未重复执行"
	report.setStage("rewards", ReportStepSkipped, summary)
	report.setStage("sweep", ReportStepSkipped, summary)
	markBuilderStepsSkipped(report, summary)
}

func (o *Orchestrator) deliverRunReport(ctx context.Context, report *RunReport, runErr error) {
	if o.notifier == nil || report == nil || !report.finalize(o.clock.Now(), runErr) {
		return
	}
	deliveryCtx, cancel := context.WithTimeout(rateLimitObservationContext(ctx), reportDeliveryTimeout)
	defer cancel()
	o.notifier.Report(deliveryCtx, *report)
}
