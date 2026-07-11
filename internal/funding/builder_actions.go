package funding

import (
	"context"
	"log/slog"

	"hyperliquid-builder-code-bot/internal/hyperliquid/exchange"
)

func (o *Orchestrator) executeClaim(ctx context.Context, state *RunState, index int) error {
	progress := &state.Builders[index].Claim
	prepared, err := o.chain.PrepareClaim(state.Builders[index].Address, o.nonce.Next())
	if err != nil {
		progress.Phase = ActionRejected
		progress.Response = sanitizeError(err)
		if saveErr := o.store.Save(ctx, *state); saveErr != nil {
			return saveErr
		}
		o.warn(ctx, "builder claim preparation failed",
			slog.String("event", "funding_action_prepare_failed"),
			slog.String("run_id", state.RunID),
			slog.String("action_kind", "claimRewards"),
		)
		o.alertOnce(ctx, state.RunID, "builder_claim_prepare_failed", "builder claim preparation failed")
		return nil
	}
	return o.submitBuilderAction(ctx, state, progress, prepared)
}

func (o *Orchestrator) executeSweep(ctx context.Context, state *RunState, index int) error {
	progress := &state.Builders[index].Sweep
	token := *state.Manifest.Token
	balance, err := o.builderBalanceAfterClaim(ctx, state.Builders[index].Address, token, state.Builders[index].Claim)
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
		return nil
	}
	progress.BalanceBefore = balance.String()
	if balance.IsZero() {
		progress.Phase = ActionZeroBalance
		return o.store.Save(ctx, *state)
	}
	prepared, err := o.chain.PrepareSpotSend(state.Builders[index].Address, state.Manifest.Settlement, token, balance, o.nonce.Next())
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
		return nil
	}
	return o.submitBuilderAction(ctx, state, progress, prepared)
}

func (o *Orchestrator) submitBuilderAction(ctx context.Context, state *RunState, progress *ActionProgress, prepared exchange.PreparedAction) error {
	preparedCopy := clonePrepared(prepared)
	progress.Prepared = &preparedCopy
	progress.Phase = ActionPrepared
	if err := o.store.Save(ctx, *state); err != nil {
		return err
	}
	o.actionEvent(ctx, state, "funding_action_prepared", prepared.Kind, ActionPrepared)
	progress.Phase = ActionSubmitting
	progress.SubmitAttempts++
	if err := o.store.Save(ctx, *state); err != nil {
		return err
	}
	o.actionEvent(ctx, state, "funding_action_submitting", prepared.Kind, ActionSubmitting)
	result, _ := o.chain.Submit(ctx, prepared)
	progress.Response = sanitizeJSON(result.Response)
	progress.Phase = submitPhase(result)
	if progress.Phase == ActionUnknown && prepared.Kind == "spotSend" {
		stop, reconcileErr := o.reconcileBuilderSweep(ctx, state, progress)
		if reconcileErr != nil {
			return reconcileErr
		}
		if stop {
			return nil
		}
	}
	if err := o.store.Save(ctx, *state); err != nil {
		return err
	}
	o.actionEvent(ctx, state, "funding_action_result", prepared.Kind, progress.Phase)
	if progress.Phase == ActionRejected || progress.Phase == ActionUnknown {
		o.alertOnce(ctx, state.RunID, "builder_action_failed", prepared.Kind+" action is "+string(progress.Phase))
	}
	return nil
}

func (o *Orchestrator) reconcileBuilderSweep(ctx context.Context, state *RunState, progress *ActionProgress) (bool, error) {
	if progress.Prepared == nil || progress.BalanceBefore == "" {
		return false, nil
	}
	query, err := o.transferQuery(state, *progress.Prepared)
	if err != nil {
		return o.recordUnconfirmedBuilderResult(ctx, state, progress, "transfer_query_validation")
	}
	evidence, matched, err := o.chain.FindSpotTransfer(ctx, query)
	if err != nil {
		return o.recordUnconfirmedBuilderResult(ctx, state, progress, "ledger_query")
	}
	after, err := o.chain.AvailableSpotBalance(ctx, progress.Prepared.Signer, *state.Manifest.Token)
	if err != nil {
		return o.recordUnconfirmedBuilderResult(ctx, state, progress, "balance_query")
	}
	progress.BalanceAfter = after.String()
	before, err := decimalFromString(progress.BalanceBefore)
	if err != nil {
		return o.recordUnconfirmedBuilderResult(ctx, state, progress, "balance_validation")
	}
	if exactEvidence(evidence, matched, query) && before.Sub(after).GreaterThanOrEqual(query.Amount) {
		copy := *evidence
		progress.Evidence = &copy
		progress.Phase = ActionAccepted
	}
	return false, nil
}
