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

func (o *Orchestrator) executePayout(ctx context.Context, state *RunState, before, amount decimal.Decimal) error {
	prepared, err := o.chain.PrepareSpotSend(
		state.Manifest.Settlement, state.Manifest.Recipient, *state.Manifest.Token, amount, o.nonce.Next(),
	)
	if err != nil {
		o.alertOnce(ctx, state.RunID, "payout_prepare_failed", "final payout preparation failed")
		return err
	}
	if err := validatePreparedPayout(state, prepared); err != nil {
		return err
	}
	state.Payout = &PayoutJournal{Prepared: clonePrepared(prepared), TotalBefore: before.String()}
	if err := o.save(ctx, state, PhasePayoutPrepared); err != nil {
		return err
	}
	return o.submitPreparedPayout(ctx, state)
}

func (o *Orchestrator) submitPreparedPayout(ctx context.Context, state *RunState) error {
	if state.Payout == nil {
		return fmt.Errorf("persisted payout journal is missing")
	}
	if err := validatePreparedPayout(state, state.Payout.Prepared); err != nil {
		return err
	}
	// This second durable boundary guarantees that the backup contains the full
	// payout intent before the request can possibly leave the process.
	if err := o.save(ctx, state, PhasePayoutSubmitting); err != nil {
		return err
	}

	result, _ := o.chain.Submit(ctx, clonePrepared(state.Payout.Prepared))
	attrs := []slog.Attr{
		slog.String("event", "funding_payout_submit_result"),
		slog.String("run_id", state.RunID),
		slog.Bool("accepted", result.Accepted),
		slog.Bool("rejected", result.Rejected),
	}
	if len(result.Response) != 0 {
		attrs = append(attrs, slog.String("response", string(result.Response)))
	}
	o.info(ctx, "final payout submission returned", attrs...)
	switch {
	case result.Accepted:
		return o.confirmPayout(ctx, state, "exchange_response")
	case result.Rejected:
		return o.rejectPayout(ctx, state)
	default:
		return o.observeUncertainPayout(ctx, state)
	}
}

func (o *Orchestrator) observeUncertainPayout(ctx context.Context, state *RunState) error {
	if state.Payout == nil {
		return fmt.Errorf("persisted payout journal is missing")
	}
	if err := validatePreparedPayout(state, state.Payout.Prepared); err != nil {
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
		if balanceErr == nil && balance.Total.LessThan(before) {
			return o.confirmPayout(ctx, state, "settlement_balance_decreased")
		}
		if attempt < payoutBalanceObservationAttempts {
			if err := o.sleeper.Sleep(ctx, payoutBalanceObservationInterval); err != nil {
				return err
			}
		}
	}
	return o.blockUncertainPayout(ctx, state,
		"final payout result remained uncertain and settlement balance did not decrease",
		"payout_ambiguous")
}

func (o *Orchestrator) confirmPayout(ctx context.Context, state *RunState, evidence string) error {
	state.BlockedReason = ""
	if err := o.save(ctx, state, PhasePayoutConfirmed); err != nil {
		return err
	}
	o.info(ctx, "final payout confirmed",
		slog.String("event", "funding_payout_confirmed"),
		slog.String("run_id", state.RunID), slog.String("evidence", evidence))
	return o.completeDatabase(ctx, state)
}

func (o *Orchestrator) rejectPayout(ctx context.Context, state *RunState) error {
	const reason = "final payout was explicitly rejected"
	state.BlockedReason = reason
	if err := o.save(ctx, state, PhaseBlocked); err != nil {
		return &FatalError{Err: fmt.Errorf("persist rejected payout state: %w", err)}
	}
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
	o.alertOnce(ctx, state.RunID, "payout_rejected", reason)
	return &FatalError{Err: fmt.Errorf("%s", reason)}
}

func (o *Orchestrator) blockUncertainPayout(ctx context.Context, state *RunState, reason, alertKey string) error {
	state.BlockedReason = reason
	if err := o.save(ctx, state, PhaseBlocked); err != nil {
		return &FatalError{Err: fmt.Errorf("persist blocked payout state: %w", err)}
	}
	if err := o.store.Archive(ctx, *state, "blocked"); err != nil {
		return &FatalError{Err: fmt.Errorf("archive blocked payout: %w", err)}
	}
	o.error(ctx, "final payout blocked",
		slog.String("event", "funding_payout_blocked"),
		slog.String("run_id", state.RunID), slog.String("reason", reason))
	o.alertOnce(ctx, state.RunID, alertKey, reason)
	return &FatalError{Err: fmt.Errorf("%s", reason)}
}
