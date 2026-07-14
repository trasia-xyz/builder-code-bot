package funding

import (
	"context"
	"fmt"
	"log/slog"
)

func (o *Orchestrator) Run(ctx context.Context, trigger Trigger) (err error) {
	report := runReport{trigger: trigger}
	defer func() { o.reportRun(ctx, report, err) }()
	o.info(ctx, "funding task started",
		slog.String("event", "funding_task_started"),
		slog.String("trigger", string(trigger)))
	current, metadata, err := o.loadCurrent(ctx)
	if err != nil {
		return err
	}
	if current != nil {
		report.state = current
		report.recordCount = len(current.Manifest.Records)
		report.recordsRead = true
		report.outcome = "completed"
		return o.recoverState(ctx, *current, metadata)
	}
	return o.runNew(ctx, trigger, &report)
}

func (o *Orchestrator) Recover(ctx context.Context) error {
	o.info(ctx, "funding recovery check started",
		slog.String("event", "funding_recovery_check_started"))
	current, metadata, err := o.loadCurrent(ctx)
	if err != nil || current == nil {
		return err
	}
	return o.recoverState(ctx, *current, metadata)
}

func (o *Orchestrator) recoverState(ctx context.Context, state RunState, metadata StateLoadMetadata) (err error) {
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
			return o.completeDatabase(ctx, &state)
		}
		return o.runPositive(ctx, &state)
	case PhasePayoutPrepared:
		// A primary payout_prepared state proves that submission never started.
		// A backup at the same phase may be one boundary behind a submitted
		// primary, so it must use balance observation instead of resubmission.
		if metadata.RecoveredFromBackup {
			return o.observeUncertainPayout(ctx, &state)
		}
		return o.submitPreparedPayout(ctx, &state)
	case PhasePayoutSubmitting:
		return o.observeUncertainPayout(ctx, &state)
	case PhasePayoutConfirmed:
		return o.completeDatabase(ctx, &state)
	case PhaseBlocked:
		if state.BlockedReason == "" {
			state.BlockedReason = "funding run is blocked"
		}
		return &FatalError{Err: fmt.Errorf("%s", state.BlockedReason)}
	default:
		return fmt.Errorf("unsupported persisted funding phase %q", state.Phase)
	}
}
