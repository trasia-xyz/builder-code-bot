package funding

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/shopspring/decimal"
)

func (o *Orchestrator) executePayout(ctx context.Context, state *RunState, before, amount decimal.Decimal) error {
	prepared, err := o.chain.PrepareSpotSend(state.Manifest.Settlement, state.Manifest.Recipient, *state.Manifest.Token, amount, o.nonce.Next())
	if err != nil {
		o.warn(ctx, "payout preparation failed",
			slog.String("event", "funding_action_prepare_failed"),
			slog.String("run_id", state.RunID),
			slog.String("action_kind", "spotSend"),
		)
		o.alertOnce(ctx, state.RunID, "payout_prepare_failed", "final payout preparation failed")
		return err
	}
	if err := validatePreparedPayout(state, prepared); err != nil {
		return err
	}
	progress := &ActionProgress{Phase: ActionPrepared, Prepared: ptrPrepared(clonePrepared(prepared)), BalanceBefore: before.String()}
	state.FinalPayout = progress
	if err := o.store.Save(ctx, *state); err != nil {
		return err
	}
	o.actionEvent(ctx, state, "funding_action_prepared", prepared.Kind, ActionPrepared)
	progress.Phase = ActionSubmitting
	progress.SubmitAttempts++
	if err := o.save(ctx, state, PhasePayoutSubmitting); err != nil {
		return err
	}
	o.actionEvent(ctx, state, "funding_action_submitting", prepared.Kind, ActionSubmitting)
	result, _ := o.chain.Submit(ctx, prepared)
	progress.Response = sanitizeJSON(result.Response)
	progress.Phase = submitPhase(result)
	if progress.Phase == ActionAccepted {
		if err := o.save(ctx, state, PhasePayoutAccepted); err != nil {
			return err
		}
		o.actionEvent(ctx, state, "funding_action_result", prepared.Kind, ActionAccepted)
		return o.completeDatabase(ctx, state)
	}
	if progress.Phase == ActionRejected {
		state.BlockedReason = "final payout was rejected"
		if err := o.save(ctx, state, PhaseBlocked); err != nil {
			return err
		}
		o.actionEvent(ctx, state, "funding_action_result", prepared.Kind, ActionRejected)
		o.alertOnce(ctx, state.RunID, "payout_rejected", state.BlockedReason)
		return fmt.Errorf("%s", state.BlockedReason)
	}
	if err := o.store.Save(ctx, *state); err != nil {
		return err
	}
	return o.reconcilePayout(ctx, state)
}

func (o *Orchestrator) reconcilePayout(ctx context.Context, state *RunState) error {
	progress := state.FinalPayout
	if progress == nil || progress.Prepared == nil {
		return fmt.Errorf("persisted final payout request is missing")
	}
	prepared := progress.Prepared
	if err := validatePreparedPayout(state, *prepared); err != nil {
		return err
	}
	amount, err := decimalFromString(prepared.Amount)
	if err != nil {
		return err
	}
	query, err := o.transferQuery(state, *prepared)
	if err != nil {
		return o.blockUnconfirmedChainResult(ctx, state, progress, "transfer_query_validation")
	}
	evidence, matched, err := o.chain.FindSpotTransfer(ctx, query)
	if err != nil {
		return o.blockUnconfirmedChainResult(ctx, state, progress, "ledger_query")
	}
	after, err := o.chain.AvailableSpotBalance(ctx, state.Manifest.Settlement, *state.Manifest.Token)
	if err != nil {
		return o.blockUnconfirmedChainResult(ctx, state, progress, "balance_query")
	}
	progress.BalanceAfter = after.String()
	before, err := decimalFromString(progress.BalanceBefore)
	if err != nil {
		return o.blockUnconfirmedChainResult(ctx, state, progress, "balance_validation")
	}
	if exactEvidence(evidence, matched, query) && before.Sub(after).GreaterThanOrEqual(amount) {
		copy := *evidence
		progress.Evidence = &copy
		progress.Phase = ActionAccepted
		if err := o.save(ctx, state, PhasePayoutAccepted); err != nil {
			return err
		}
		return o.completeDatabase(ctx, state)
	}
	if progress.SubmitAttempts < 2 {
		progress.Phase = ActionSubmitting
		progress.SubmitAttempts++
		if err := o.save(ctx, state, PhasePayoutSubmitting); err != nil {
			return err
		}
		result, _ := o.chain.Submit(ctx, clonePrepared(*prepared))
		progress.Response = sanitizeJSON(result.Response)
		progress.Phase = submitPhase(result)
		if progress.Phase == ActionAccepted {
			if err := o.save(ctx, state, PhasePayoutAccepted); err != nil {
				return err
			}
			return o.completeDatabase(ctx, state)
		}
		if progress.Phase == ActionRejected {
			// A replay rejection does not prove the original ambiguous request
			// failed; the same nonce may already have been accepted.
			progress.Phase = ActionUnknown
			if err := o.store.Save(ctx, *state); err != nil {
				return err
			}
			return o.reconcilePayout(ctx, state)
		}
		if err := o.store.Save(ctx, *state); err != nil {
			return err
		}
		return o.reconcilePayout(ctx, state)
	}
	state.BlockedReason = "final payout result remains ambiguous"
	if err := o.save(ctx, state, PhaseBlocked); err != nil {
		return err
	}
	o.alertOnce(ctx, state.RunID, "payout_ambiguous", state.BlockedReason)
	return fmt.Errorf("%s", state.BlockedReason)
}

func (o *Orchestrator) blockUnconfirmedChainResult(ctx context.Context, state *RunState, progress *ActionProgress, stage string) error {
	progress.Phase = ActionUnknown
	state.BlockedReason = "chain result could not be confirmed"
	if err := o.save(ctx, state, PhaseBlocked); err != nil {
		return err
	}
	o.warn(ctx, "chain result could not be confirmed",
		slog.String("event", "chain_result_unconfirmed"),
		slog.String("run_id", state.RunID),
		slog.String("stage", stage),
	)
	o.alertOnce(ctx, state.RunID, "chain_result_unconfirmed", state.BlockedReason)
	return fmt.Errorf("%s", state.BlockedReason)
}

func (o *Orchestrator) recordUnconfirmedBuilderResult(ctx context.Context, state *RunState, progress *ActionProgress, stage string) (bool, error) {
	progress.Phase = ActionUnknown
	if err := o.store.Save(ctx, *state); err != nil {
		return true, err
	}
	o.warn(ctx, "builder chain result could not be confirmed",
		slog.String("event", "chain_result_unconfirmed"),
		slog.String("run_id", state.RunID),
		slog.String("stage", stage),
	)
	o.alertOnce(ctx, state.RunID, "chain_result_unconfirmed", "builder chain result could not be confirmed")
	return true, nil
}
