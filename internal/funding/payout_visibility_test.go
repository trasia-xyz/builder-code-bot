package funding

import (
	"context"
	"errors"
	"testing"

	"builder-code-bot/internal/hyperliquid/info"

	"github.com/shopspring/decimal"
)

func TestUncertainPayoutRequiresConsecutiveExpectedPostBalance(t *testing.T) {
	fx := newFixture(t)
	state, report := payoutVisibilityState(t, fx, PhasePayoutSubmitting, "2")
	expected := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("0.5"), Available: decimal.RequireFromString("0.5"),
	}
	chain := &payoutVisibilityChain{
		fakeChain: fx.chain,
		sleeper:   fx.sleeper,
		results: []payoutVisibilityResult{
			// Historical lower totals must not prove this payout happened.
			{balance: info.SpotBalanceAmounts{Total: decimal.Zero, Available: decimal.Zero}},
			{balance: expected},
			// A contradictory sample resets the confirmation streak.
			{balance: info.SpotBalanceAmounts{Total: decimal.NewFromInt(2), Available: decimal.NewFromInt(2)}},
			{balance: expected},
			{balance: expected},
		},
	}
	fx.orchestrator.chain = chain

	if err := fx.orchestrator.observeUncertainPayout(
		context.Background(), state, testActiveTarget(t, state), report,
	); err != nil {
		t.Fatal(err)
	}
	if chain.calls != 5 {
		t.Fatalf("settlement balance calls = %d, want 5", chain.calls)
	}
	referralPayout := report.target(PayoutKindReferral).Payout
	if referralPayout.Status != ReportStepSuccess ||
		referralPayout.ConfirmationEvidence != "settlement_balance_decreased" {
		t.Fatalf("payout report = %#v", referralPayout)
	}
	if len(fx.chain.events) != 0 {
		t.Fatalf("uncertain observation submitted chain action: %v", fx.chain.events)
	}
	if !equalUint64(fx.repo.completedIDs, []uint64{1, 2}) {
		t.Fatalf("completed IDs = %v", fx.repo.completedIDs)
	}
}

func TestUncertainPayoutPostStateFromFirstSampleConfirms(t *testing.T) {
	fx := newFixture(t)
	state, report := payoutVisibilityState(t, fx, PhasePayoutSubmitting, "2")
	expected := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("0.5"), Available: decimal.RequireFromString("0.5"),
	}
	results := make([]payoutVisibilityResult, payoutBalanceObservationAttempts)
	for index := range results {
		results[index].balance = expected
	}
	chain := &payoutVisibilityChain{
		fakeChain: fx.chain, sleeper: fx.sleeper, results: results,
	}
	fx.orchestrator.chain = chain

	err := fx.orchestrator.observeUncertainPayout(
		context.Background(), state, testActiveTarget(t, state), report,
	)
	if err != nil || fx.store.current != nil ||
		!equalUint64(fx.repo.completedIDs, []uint64{1, 2}) {
		t.Fatalf("observation error=%v current=%#v completed=%v", err, fx.store.current, fx.repo.completedIDs)
	}
}

func TestUncertainPayoutQueryErrorsDoNotConsumeSuccessfulObservations(t *testing.T) {
	fx := newFixture(t)
	state, report := payoutVisibilityState(t, fx, PhasePayoutSubmitting, "2")
	baseline := info.SpotBalanceAmounts{Total: decimal.NewFromInt(2), Available: decimal.NewFromInt(2)}
	expected := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("0.5"), Available: decimal.RequireFromString("0.5"),
	}
	const queryErrors = 3
	results := make([]payoutVisibilityResult, 0, queryErrors+payoutBalanceObservationAttempts)
	for range queryErrors {
		results = append(results, payoutVisibilityResult{err: errors.New("temporary balance query failure")})
	}
	for range payoutBalanceObservationAttempts - payoutBalanceRequiredConfirmations {
		results = append(results, payoutVisibilityResult{balance: baseline})
	}
	for range payoutBalanceRequiredConfirmations {
		results = append(results, payoutVisibilityResult{balance: expected})
	}
	chain := &payoutVisibilityChain{
		fakeChain: fx.chain, sleeper: fx.sleeper, results: results,
	}
	fx.orchestrator.chain = chain

	if err := fx.orchestrator.observeUncertainPayout(
		context.Background(), state, testActiveTarget(t, state), report,
	); err != nil {
		t.Fatal(err)
	}
	wantCalls := queryErrors + payoutBalanceObservationAttempts
	if chain.calls != wantCalls {
		t.Fatalf("settlement balance calls = %d, want %d", chain.calls, wantCalls)
	}
	if len(chain.delaysAtCalls) != wantCalls || chain.delaysAtCalls[0] != 1 {
		t.Fatalf("delays observed at balance calls = %v", chain.delaysAtCalls)
	}
	for index, delayCount := range chain.delaysAtCalls {
		if delayCount != index+1 {
			t.Fatalf("balance call %d saw %d waits, want %d", index+1, delayCount, index+1)
		}
	}
	if len(fx.chain.events) != 0 {
		t.Fatalf("uncertain observation submitted chain action: %v", fx.chain.events)
	}
}

func TestUncertainPayoutConfirmsWithoutRecipientCredit(t *testing.T) {
	fx := newFixture(t)
	state, report := payoutVisibilityState(t, fx, PhasePayoutSubmitting, "2")
	fx.chain.balances["0xrecipient"] = decimal.Zero
	before := info.SpotBalanceAmounts{
		Total: decimal.NewFromInt(2), Available: decimal.NewFromInt(2),
	}
	expected := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("0.5"), Available: decimal.RequireFromString("0.5"),
	}
	results := []payoutVisibilityResult{{balance: before}}
	for range payoutBalanceObservationAttempts - 1 {
		results = append(results, payoutVisibilityResult{balance: expected})
	}
	chain := &payoutVisibilityChain{
		fakeChain: fx.chain, sleeper: fx.sleeper, results: results,
	}
	fx.orchestrator.chain = chain

	err := fx.orchestrator.observeUncertainPayout(
		context.Background(), state, testActiveTarget(t, state), report,
	)
	if err != nil || fx.store.current != nil ||
		!equalUint64(fx.repo.completedIDs, []uint64{1, 2}) {
		t.Fatalf("observation error=%v current=%#v", err, fx.store.current)
	}
	if len(fx.chain.events) != 0 {
		t.Fatalf("events=%v completed=%v", fx.chain.events, fx.repo.completedIDs)
	}
}

func TestBlockedUncertainPayoutRecoveryRemainsTerminalWithoutObservation(t *testing.T) {
	fx := newFixture(t)
	state, report := payoutVisibilityState(t, fx, PhaseBlocked, "2")
	target := testActiveTarget(t, state)
	target.Payout.SubmitAttempts = 2
	target.BlockedReason = uncertainPayoutBlockedReasonPrefix + " after exhausted replay"
	fx.store.current = state
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("0.5")
	chain := &payoutVisibilityChain{fakeChain: fx.chain, sleeper: fx.sleeper}
	fx.orchestrator.chain = chain

	err := fx.orchestrator.recoverState(
		context.Background(), state, StateLoadMetadata{}, report,
	)
	if err == nil || !IsFatal(err) {
		t.Fatalf("recoverState() error = %v, want terminal blocked result", err)
	}
	if len(fx.chain.events) != 0 || chain.calls != 0 {
		t.Fatalf("blocked recovery calls=%d events=%v", chain.calls, fx.chain.events)
	}
	if fx.store.current == nil || fx.store.archiveResult != "" || len(fx.repo.completedIDs) != 0 {
		t.Fatalf("recovered current=%#v archive=%q completed=%v",
			fx.store.current, fx.store.archiveResult, fx.repo.completedIDs)
	}
}

func TestExplicitlyRejectedBlockedPayoutRemainsTerminal(t *testing.T) {
	fx := newFixture(t)
	state, report := payoutVisibilityState(t, fx, PhaseBlocked, "2")
	testActiveTarget(t, state).BlockedReason = explicitlyRejectedPayoutBlockedReason
	chain := &payoutVisibilityChain{fakeChain: fx.chain, sleeper: fx.sleeper}
	fx.orchestrator.chain = chain

	err := fx.orchestrator.recoverState(
		context.Background(), state, StateLoadMetadata{}, report,
	)
	if err == nil || !IsFatal(err) {
		t.Fatalf("recoverState() error = %v, want fatal rejection", err)
	}
	if chain.calls != 0 || len(fx.chain.events) != 0 {
		t.Fatalf("rejected recovery balance calls=%d events=%v", chain.calls, fx.chain.events)
	}
}

type payoutVisibilityResult struct {
	balance info.SpotBalanceAmounts
	err     error
}

type payoutVisibilityChain struct {
	*fakeChain
	sleeper       *fakeSleeper
	results       []payoutVisibilityResult
	calls         int
	delaysAtCalls []int
}

func (c *payoutVisibilityChain) SpotBalance(
	ctx context.Context,
	address string,
	token info.Token,
) (info.SpotBalanceAmounts, error) {
	if address != "0xsettlement" {
		return c.fakeChain.SpotBalance(ctx, address, token)
	}
	c.calls++
	c.delaysAtCalls = append(c.delaysAtCalls, len(c.sleeper.delays))
	if len(c.results) == 0 {
		return c.fakeChain.SpotBalance(ctx, address, token)
	}
	result := c.results[0]
	c.results = c.results[1:]
	return result.balance, result.err
}

func payoutVisibilityState(
	t *testing.T,
	fx *fixture,
	phase Phase,
	totalBefore string,
) (*RunState, *RunReport) {
	t.Helper()
	state := fx.positiveState(t, phase)
	target := testTarget(t, &state, PayoutKindReferral)
	target.Payout = &PayoutJournal{
		Prepared: validPayout(t, state, 901), TotalBefore: totalBefore, SubmitAttempts: 1,
	}
	fx.chain.balances["0xrecipient"] = decimal.RequireFromString(target.Manifest.PayoutTotal)
	report := newRunReport(
		fx.clock.now, state.Trigger, ReportExecution{}, fx.orchestrator.network,
		fx.orchestrator.builders, fx.orchestrator.builderNames,
		target.Manifest.Settlement, target.Manifest.Recipient,
	)
	report.Outcome = "completed"
	report.syncState(&state)
	return &state, &report
}
