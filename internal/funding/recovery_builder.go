package funding

import (
	"context"
	"fmt"
	"log/slog"

	"hyperliquid-builder-code-bot/internal/hyperliquid/exchange"
)

func (o *Orchestrator) resumeBuilderAction(ctx context.Context, state *RunState, builderAddress, expectedKind string, progress *ActionProgress, prepare func() (exchange.PreparedAction, error)) error {
	if progress.Unconfirmable {
		if expectedKind != "claimRewards" {
			return fmt.Errorf("only persisted claims may be unconfirmable")
		}
		progress.Phase = ActionUnknown
		return nil
	}
	wasAmbiguous := progress.Phase == ActionSubmitting || progress.Phase == ActionUnknown
	switch progress.Phase {
	case ActionAccepted, ActionZeroBalance:
		return nil
	case ActionRejected:
		// A rejected action without a prepared request failed locally before
		// submission, so recovery may safely build its first request. A persisted
		// prepared request means the exchange explicitly rejected that request and
		// requires operator review before a new nonce is created.
		if progress.Prepared != nil {
			return nil
		}
		progress.Phase = ""
		progress.Response = nil
		fallthrough
	case "":
		if prepare == nil {
			return fmt.Errorf("missing builder action preparer")
		}
		prepared, err := prepare()
		if err != nil {
			progress.Phase = ActionRejected
			progress.Response = sanitizeError(err)
			if saveErr := o.store.Save(ctx, *state); saveErr != nil {
				return saveErr
			}
			o.warn(ctx, "builder claim preparation failed",
				slog.String("event", "funding_action_prepare_failed"),
				slog.String("run_id", state.RunID),
				slog.String("action_kind", expectedKind),
			)
			o.alertOnce(ctx, state.RunID, "builder_claim_prepare_failed", "builder claim preparation failed")
			return nil
		}
		copy := clonePrepared(prepared)
		progress.Prepared = &copy
		progress.Phase = ActionPrepared
		if err := o.store.Save(ctx, *state); err != nil {
			return err
		}
		o.actionEvent(ctx, state, "funding_action_prepared", expectedKind, ActionPrepared)
	case ActionSubmitting, ActionUnknown:
		if progress.Prepared != nil && progress.Prepared.Kind == "spotSend" {
			stop, err := o.reconcileBuilderSweep(ctx, state, progress)
			if err != nil {
				return err
			}
			if stop {
				return nil
			}
			if progress.Phase == ActionAccepted {
				return o.store.Save(ctx, *state)
			}
		}
	}
	if progress.Prepared == nil {
		return fmt.Errorf("persisted builder action request is missing")
	}
	if err := validatePreparedBuilder(state, builderAddress, expectedKind, progress); err != nil {
		return err
	}
	if progress.SubmitAttempts >= 2 {
		progress.Phase = ActionUnknown
		if err := o.store.Save(ctx, *state); err != nil {
			return err
		}
		o.actionEvent(ctx, state, "funding_action_result", expectedKind, ActionUnknown)
		o.alertOnce(ctx, state.RunID, "builder_action_failed", expectedKind+" action is "+string(ActionUnknown))
		return nil
	}
	progress.Phase = ActionSubmitting
	progress.SubmitAttempts++
	if err := o.store.Save(ctx, *state); err != nil {
		return err
	}
	o.actionEvent(ctx, state, "funding_action_submitting", expectedKind, ActionSubmitting)
	result, _ := o.chain.Submit(ctx, clonePrepared(*progress.Prepared))
	progress.Response = sanitizeJSON(result.Response)
	progress.Phase = submitPhase(result)
	if wasAmbiguous && progress.Phase == ActionRejected {
		// A same-nonce replay rejection cannot distinguish an earlier accepted
		// request from a definitively rejected original request.
		progress.Phase = ActionUnknown
		if expectedKind == "spotSend" {
			stop, err := o.reconcileBuilderSweep(ctx, state, progress)
			if err != nil {
				return err
			}
			if stop {
				return nil
			}
		}
	}
	if err := o.store.Save(ctx, *state); err != nil {
		return err
	}
	o.actionEvent(ctx, state, "funding_action_result", expectedKind, progress.Phase)
	if progress.Phase == ActionRejected || progress.Phase == ActionUnknown {
		o.alertOnce(ctx, state.RunID, "builder_action_failed", expectedKind+" action is "+string(progress.Phase))
	}
	return nil
}
