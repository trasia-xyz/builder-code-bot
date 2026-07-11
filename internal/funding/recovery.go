package funding

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"hyperliquid-builder-code-bot/internal/hyperliquid/exchange"
	"hyperliquid-builder-code-bot/internal/hyperliquid/info"

	"github.com/shopspring/decimal"
)

const (
	ledgerClockSkew    = 5 * time.Second
	ledgerFutureMargin = 30 * time.Second
)

func (o *Orchestrator) Run(ctx context.Context, trigger Trigger) (err error) {
	report := runReport{trigger: trigger}
	defer func() { o.reportRun(ctx, report, err) }()
	current, metadata, err := o.loadCurrent(ctx)
	if err != nil {
		return err
	}
	if current != nil {
		if err := o.recoverState(ctx, *current, metadata); err != nil {
			return err
		}
	}
	return o.runNew(ctx, trigger, &report)
}

func (o *Orchestrator) Recover(ctx context.Context) error {
	current, metadata, err := o.loadCurrent(ctx)
	if err != nil || current == nil {
		return err
	}
	return o.recoverState(ctx, *current, metadata)
}

func (o *Orchestrator) recoverState(ctx context.Context, state RunState, metadata StateLoadMetadata) (err error) {
	if metadata.RecoveredFromBackup {
		unconfirmableClaims := markBackupPreparedActionsAmbiguous(&state)
		if unconfirmableClaims > 0 {
			o.warn(ctx, "backup claims cannot be confirmed",
				slog.String("event", "backup_claim_unconfirmable"),
				slog.String("run_id", state.RunID),
				slog.Int("claim_count", unconfirmableClaims),
			)
			o.alertOnce(ctx, state.RunID, "backup_claim_unconfirmable", "backup claims cannot be safely confirmed or replayed")
		}
	}
	o.info(ctx, "funding recovery started",
		slog.String("event", "recovery_started"),
		slog.String("run_id", state.RunID),
		slog.String("phase", string(state.Phase)),
	)
	defer func() {
		if err == nil {
			o.info(ctx, "funding recovery completed",
				slog.String("event", "recovery_completed"),
				slog.String("run_id", state.RunID),
			)
			return
		}
		o.warn(ctx, "funding recovery failed",
			slog.String("event", "recovery_failed"),
			slog.String("run_id", state.RunID),
			slog.String("phase", string(state.Phase)),
		)
		o.alertOnce(ctx, state.RunID, "recovery_failed", "funding recovery failed")
	}()
	if state.Phase == PhaseCompleted {
		if err := o.store.Archive(ctx, state, "completed"); err != nil {
			return o.databaseFailure(ctx, &state, err)
		}
		if err := o.store.Clear(ctx); err != nil {
			return o.databaseFailure(ctx, &state, err)
		}
		o.forgetAlerts(state.RunID)
		return nil
	}
	switch state.Phase {
	case PhasePayoutAccepted, PhaseDBUpdating:
		return o.completeDatabase(ctx, &state)
	case PhasePayoutSubmitting:
		if state.FinalPayout != nil && state.FinalPayout.Phase == ActionAccepted {
			if err := o.save(ctx, &state, PhasePayoutAccepted); err != nil {
				return err
			}
			return o.completeDatabase(ctx, &state)
		}
		if state.FinalPayout != nil && state.FinalPayout.Phase == ActionPrepared {
			return o.submitExistingPayout(ctx, &state)
		}
		return o.reconcilePayout(ctx, &state)
	case PhaseBlocked:
		if state.FinalPayout != nil {
			if state.FinalPayout.Phase == ActionPrepared {
				return o.submitExistingPayout(ctx, &state)
			}
			if state.FinalPayout.Phase == ActionUnknown || state.FinalPayout.Phase == ActionSubmitting {
				return o.reconcilePayout(ctx, &state)
			}
			return fmt.Errorf("funding run remains blocked: %s", state.BlockedReason)
		}
		return o.resumeConsolidation(ctx, &state)
	case PhaseFunded:
		if state.FinalPayout != nil {
			switch state.FinalPayout.Phase {
			case ActionPrepared:
				return o.submitExistingPayout(ctx, &state)
			case ActionSubmitting, ActionUnknown:
				return o.reconcilePayout(ctx, &state)
			case ActionAccepted:
				if err := o.save(ctx, &state, PhasePayoutAccepted); err != nil {
					return err
				}
				return o.completeDatabase(ctx, &state)
			default:
				return fmt.Errorf("unsupported payout action phase %q", state.FinalPayout.Phase)
			}
		}
		return o.finishConsolidation(ctx, &state)
	case PhasePrepared, PhaseConsolidating:
		return o.resumeConsolidation(ctx, &state)
	default:
		return fmt.Errorf("unsupported persisted funding phase %q", state.Phase)
	}
}

func markBackupPreparedActionsAmbiguous(state *RunState) int {
	unconfirmableClaims := 0
	for i := range state.Builders {
		claim := &state.Builders[i].Claim
		if claim.Phase == ActionPrepared && claim.Prepared != nil {
			claim.Phase = ActionUnknown
			if claim.SubmitAttempts < 1 {
				claim.SubmitAttempts = 1
			}
			claim.Unconfirmable = true
			unconfirmableClaims++
		}
		sweep := &state.Builders[i].Sweep
		if sweep.Phase == ActionPrepared && sweep.Prepared != nil {
			sweep.Phase = ActionUnknown
			if sweep.SubmitAttempts < 1 {
				sweep.SubmitAttempts = 1
			}
		}
	}
	if state.FinalPayout != nil && state.FinalPayout.Phase == ActionPrepared && state.FinalPayout.Prepared != nil {
		state.FinalPayout.Phase = ActionUnknown
		if state.FinalPayout.SubmitAttempts < 1 {
			state.FinalPayout.SubmitAttempts = 1
		}
	}
	return unconfirmableClaims
}

func (o *Orchestrator) submitExistingPayout(ctx context.Context, state *RunState) error {
	progress := state.FinalPayout
	if progress == nil || progress.Prepared == nil {
		return fmt.Errorf("persisted final payout request is missing")
	}
	if err := validatePreparedPayout(state, *progress.Prepared); err != nil {
		return err
	}
	progress.Phase = ActionSubmitting
	progress.SubmitAttempts++
	if err := o.save(ctx, state, PhasePayoutSubmitting); err != nil {
		return err
	}
	result, _ := o.chain.Submit(ctx, clonePrepared(*progress.Prepared))
	progress.Response = sanitizeJSON(result.Response)
	progress.Phase = submitPhase(result)
	switch progress.Phase {
	case ActionAccepted:
		if err := o.save(ctx, state, PhasePayoutAccepted); err != nil {
			return err
		}
		return o.completeDatabase(ctx, state)
	case ActionRejected:
		state.BlockedReason = "final payout was rejected"
		if err := o.save(ctx, state, PhaseBlocked); err != nil {
			return err
		}
		o.actionEvent(ctx, state, "funding_action_result", "spotSend", ActionRejected)
		o.alertOnce(ctx, state.RunID, "payout_rejected", state.BlockedReason)
		return fmt.Errorf("%s", state.BlockedReason)
	default:
		if err := o.store.Save(ctx, *state); err != nil {
			return err
		}
		return o.reconcilePayout(ctx, state)
	}
}

func (o *Orchestrator) resumeConsolidation(ctx context.Context, state *RunState) error {
	if state.Manifest.PayoutTotal == "0" {
		return o.completeDatabase(ctx, state)
	}
	if state.Manifest.Token == nil {
		return fmt.Errorf("positive persisted manifest has no canonical token")
	}
	if err := o.save(ctx, state, PhaseConsolidating); err != nil {
		return err
	}
	for i := range state.Builders {
		if err := o.resumeBuilderAction(ctx, state, state.Builders[i].Address, "claimRewards", &state.Builders[i].Claim, func() (exchange.PreparedAction, error) {
			return o.chain.PrepareClaim(state.Builders[i].Address, o.nonce.Next())
		}); err != nil {
			return err
		}
	}
	for i := range state.Builders {
		progress := &state.Builders[i].Sweep
		// An unknown sweep without a prepared request means the balance query
		// failed before any chain mutation was prepared. It is safe to retry
		// that observation and create the first request during recovery.
		if progress.Phase == "" || progress.Phase == ActionZeroBalance ||
			(progress.Phase == ActionUnknown || progress.Phase == ActionRejected) && progress.Prepared == nil {
			balance, err := o.builderBalanceAfterClaim(ctx, state.Builders[i].Address, *state.Manifest.Token, state.Builders[i].Claim)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				progress.Phase = ActionUnknown
				progress.Response = sanitizeError(err)
				if saveErr := o.store.Save(ctx, *state); saveErr != nil {
					return saveErr
				}
				o.warn(ctx, "builder balance query failed",
					slog.String("event", "funding_builder_balance_failed"),
					slog.String("run_id", state.RunID),
				)
				o.alertOnce(ctx, state.RunID, "builder_balance_failed", "builder balance query failed")
				continue
			}
			progress.BalanceBefore = balance.String()
			if balance.IsZero() {
				progress.Phase = ActionZeroBalance
				if err := o.store.Save(ctx, *state); err != nil {
					return err
				}
				continue
			}
			prepared, err := o.chain.PrepareSpotSend(state.Builders[i].Address, state.Manifest.Settlement, *state.Manifest.Token, balance, o.nonce.Next())
			if err != nil {
				progress.Phase = ActionRejected
				progress.Response = sanitizeError(err)
				if saveErr := o.store.Save(ctx, *state); saveErr != nil {
					return saveErr
				}
				o.warn(ctx, "builder sweep preparation failed",
					slog.String("event", "funding_action_prepare_failed"),
					slog.String("run_id", state.RunID),
					slog.String("action_kind", "spotSend"),
				)
				o.alertOnce(ctx, state.RunID, "builder_sweep_prepare_failed", "builder sweep preparation failed")
				continue
			}
			preparedCopy := clonePrepared(prepared)
			progress.Prepared = &preparedCopy
			progress.Phase = ActionPrepared
			if err := o.store.Save(ctx, *state); err != nil {
				return err
			}
			o.actionEvent(ctx, state, "funding_action_prepared", "spotSend", ActionPrepared)
		}
		if err := o.resumeBuilderAction(ctx, state, state.Builders[i].Address, "spotSend", progress, nil); err != nil {
			return err
		}
	}
	return o.finishConsolidation(ctx, state)
}

func (o *Orchestrator) finishConsolidation(ctx context.Context, state *RunState) error {
	if state.Manifest.Token == nil {
		return fmt.Errorf("positive persisted manifest has no canonical token")
	}
	balance, err := o.chain.AvailableSpotBalance(ctx, state.Manifest.Settlement, *state.Manifest.Token)
	if err != nil {
		o.alertOnce(ctx, state.RunID, "settlement_balance_failed", "settlement balance query failed")
		return err
	}
	payout, err := decimalFromString(state.Manifest.PayoutTotal)
	if err != nil {
		return err
	}
	if balance.LessThan(payout) {
		state.BlockedReason = fmt.Sprintf("settlement balance %s is below payout %s", balance, payout)
		if err := o.save(ctx, state, PhaseBlocked); err != nil {
			return err
		}
		o.alertOnce(ctx, state.RunID, "settlement_underfunded", "settlement balance is below required payout")
		return fmt.Errorf("%s", state.BlockedReason)
	}
	state.BlockedReason = ""
	if err := o.save(ctx, state, PhaseFunded); err != nil {
		return err
	}
	if state.FinalPayout != nil {
		return fmt.Errorf("persisted payout exists in funded phase")
	}
	return o.executePayout(ctx, state, balance, payout)
}

func (o *Orchestrator) transferQuery(state *RunState, action exchange.PreparedAction) (info.TransferQuery, error) {
	amount, err := decimal.NewFromString(action.Amount)
	if err != nil {
		return info.TransferQuery{}, fmt.Errorf("parse persisted transfer amount: %w", err)
	}
	if state.Manifest.Token == nil {
		return info.TransferQuery{}, fmt.Errorf("persisted manifest token is missing")
	}
	actionTime := action.Nonce
	skewMillis := uint64(ledgerClockSkew / time.Millisecond)
	start := uint64(0)
	if actionTime > skewMillis {
		start = actionTime - skewMillis
	}
	nowMillis := o.clock.Now().UnixMilli()
	if nowMillis < 0 {
		return info.TransferQuery{}, fmt.Errorf("current time is before unix epoch")
	}
	end := uint64(nowMillis)
	marginMillis := uint64(ledgerFutureMargin / time.Millisecond)
	if end > ^uint64(0)-marginMillis {
		end = ^uint64(0)
	} else {
		end += marginMillis
	}
	return info.TransferQuery{
		Sender: action.Signer, Destination: action.Destination,
		TokenName: state.Manifest.Token.Name, Amount: amount,
		ActionTime: actionTime, StartTime: start, EndTime: end,
	}, nil
}

func exactEvidence(evidence *info.LedgerUpdate, matched bool, query info.TransferQuery) bool {
	if !matched || evidence == nil {
		return false
	}
	_, exact := info.MatchSpotTransfer([]info.LedgerUpdate{*evidence}, query)
	return exact
}
