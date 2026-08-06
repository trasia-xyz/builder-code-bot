package funding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"builder-code-bot/internal/hyperliquid/exchange"
	"builder-code-bot/internal/hyperliquid/info"

	"github.com/shopspring/decimal"
)

func TestAmbiguousPayoutUsesOnlyExactSettlementTotalDecrease(t *testing.T) {
	tests := []struct {
		name          string
		totalBefore   string
		activationFee string
		totalAfter    string
		hold          string
		evidence      string
	}{
		{
			name: "payout amount", totalBefore: "2", activationFee: "0",
			totalAfter: "0.5", hold: "0.1", evidence: "settlement_balance_decreased",
		},
		{
			name: "payout amount plus new address fee", totalBefore: "2.5",
			activationFee: "1", totalAfter: "0", hold: "0",
			evidence: "settlement_balance_decreased_with_activation_fee",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.chain.balances["0xsettlement"] = decimal.RequireFromString(test.totalBefore)
			fx.chain.holds["0xsettlement"] = decimal.RequireFromString(test.hold)
			// A recipient may move the payout away immediately. Its Info API must
			// therefore have no bearing on payout confirmation (or preparation).
			fx.chain.balanceErrors["0xrecipient"] = errors.New("recipient balance unavailable")
			chain := newPayoutReplayChain(fx.chain, map[string][]payoutReplayStep{
				"0xrecipient": {{
					result: exchange.SubmitResult{}, apply: true,
					activationFee: decimal.RequireFromString(test.activationFee),
				}},
			})
			fx.orchestrator.chain = chain

			if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
				t.Fatal(err)
			}

			actions := settlementPayoutActions(fx.chain.submittedActions)
			if len(actions) != 1 {
				t.Fatalf("settlement payout submissions = %d, want 1: %#v", len(actions), actions)
			}
			if got := fx.chain.balanceCalls["0xrecipient"]; got != 0 {
				t.Fatalf("recipient balance calls = %d, want 0", got)
			}
			if got := fx.chain.balances["0xrecipient"]; !got.IsZero() {
				t.Fatalf("recipient retained balance = %s, want 0", got)
			}
			if !equalUint64(fx.repo.completedIDs, []uint64{1, 2}) {
				t.Fatalf("completed IDs = %v", fx.repo.completedIDs)
			}
			report := onlyReport(t, fx.notifier)
			payout := report.target(PayoutKindPointsRebate).Payout
			if payout.Status != ReportStepSuccess ||
				payout.ConfirmationEvidence != test.evidence {
				t.Fatalf("payout report = %#v", payout)
			}
			if !report.SettlementBalance.Observed ||
				report.SettlementBalance.Total != test.totalAfter {
				t.Fatalf("settlement balance report = %#v", report.SettlementBalance)
			}
		})
	}
}

func TestAmbiguousPayoutReplaysTheExactPreparedRequestAtMostOnce(t *testing.T) {
	tests := []struct {
		name     string
		firstErr error
	}{
		{name: "unclear response"},
		{name: "network timeout", firstErr: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
			fx.chain.balanceErrors["0xrecipient"] = errors.New("recipient balance unavailable")
			chain := newPayoutReplayChain(fx.chain, map[string][]payoutReplayStep{
				"0xrecipient": {
					{result: exchange.SubmitResult{}, err: test.firstErr},
					{result: exchange.SubmitResult{Accepted: true}, apply: true},
				},
			})
			chain.store = fx.store
			fx.orchestrator.chain = chain

			if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
				t.Fatal(err)
			}

			assertOneExactPayoutReplay(t, chain, "0xrecipient")
			if got := chain.prepareCalls["0xrecipient"]; got != 1 {
				t.Fatalf("PrepareSpotSend calls = %d, want 1", got)
			}
			if !equalUint64(fx.repo.completedIDs, []uint64{1, 2}) {
				t.Fatalf("completed IDs = %v", fx.repo.completedIDs)
			}
		})
	}
}

func TestDuplicateNonceReplayResponseRemainsAmbiguousUntilSettlementDecreaseIsVisible(t *testing.T) {
	fx := newFixture(t)
	before := decimal.NewFromInt(2)
	fx.chain.balances["0xsettlement"] = before
	fx.chain.balanceErrors["0xrecipient"] = errors.New("recipient balance unavailable")
	chain := newPayoutReplayChain(fx.chain, map[string][]payoutReplayStep{
		"0xrecipient": {
			// The first request is applied, but its response and indexed balance
			// are unavailable throughout the first observation window.
			{result: exchange.SubmitResult{}, apply: true},
			{result: duplicateNonceResult(1786010960000)},
		},
	})
	chain.store = fx.store
	chain.hideSettlementUntilSubmissions = 2
	chain.hiddenSettlement = info.SpotBalanceAmounts{Total: before, Available: before}
	fx.orchestrator.chain = chain

	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}

	assertOneExactPayoutReplay(t, chain, "0xrecipient")
	if !equalUint64(fx.repo.completedIDs, []uint64{1, 2}) || fx.store.current != nil {
		t.Fatalf("completed=%v current=%#v", fx.repo.completedIDs, fx.store.current)
	}
	report := onlyReport(t, fx.notifier)
	if payout := report.target(PayoutKindPointsRebate).Payout; payout.Status != ReportStepSuccess ||
		payout.ConfirmationEvidence != "settlement_balance_decreased" {
		t.Fatalf("payout report = %#v", payout)
	}
}

func TestAmbiguousPayoutStopsAfterOneFailedExactReplayAndReportsBlocked(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	fx.chain.balanceErrors["0xrecipient"] = errors.New("recipient balance unavailable")
	chain := newPayoutReplayChain(fx.chain, map[string][]payoutReplayStep{
		"0xrecipient": {
			{result: exchange.SubmitResult{}},
			{result: duplicateNonceResult(1786010960000)},
		},
	})
	chain.store = fx.store
	fx.orchestrator.chain = chain

	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || !IsFatal(err) {
		t.Fatalf("Run() error = %v, want fatal blocked result", err)
	}
	assertOneExactPayoutReplay(t, chain, "0xrecipient")
	if got := chain.prepareCalls["0xrecipient"]; got != 1 {
		t.Fatalf("PrepareSpotSend calls = %d, want 1", got)
	}
	if fx.store.current == nil ||
		testTarget(t, fx.store.current, PayoutKindPointsRebate).Phase != PhaseBlocked ||
		fx.store.archiveResult != "blocked" || len(fx.repo.completedIDs) != 0 {
		t.Fatalf("current=%#v archive=%q completed=%v",
			fx.store.current, fx.store.archiveResult, fx.repo.completedIDs)
	}
	report := onlyReport(t, fx.notifier)
	if report.Outcome != "blocked" ||
		report.target(PayoutKindPointsRebate).Payout.Status != ReportStepFailed {
		t.Fatalf("blocked report = %#v", report)
	}
	if stage := report.stage("finalize"); stage == nil || stage.Status != ReportStepSuccess {
		t.Fatalf("finalize stage = %#v", stage)
	}

	// The exhausted replay budget is durable: later recovery may observe but
	// must never submit a third request or prepare a new nonce.
	fx.notifier.reports = nil
	err = fx.orchestrator.Recover(context.Background())
	if err == nil || !IsFatal(err) {
		t.Fatalf("Recover() error = %v, want retained blocked result", err)
	}
	assertOneExactPayoutReplay(t, chain, "0xrecipient")
	if got := chain.prepareCalls["0xrecipient"]; got != 1 {
		t.Fatalf("PrepareSpotSend calls after recovery = %d, want 1", got)
	}
}

func TestReplayTransportFailureOrUnclearResponseAlsoEndsBlocked(t *testing.T) {
	tests := []struct {
		name       string
		replayStep payoutReplayStep
	}{
		{name: "network failure", replayStep: payoutReplayStep{err: errors.New("network unavailable")}},
		{name: "unclear response", replayStep: payoutReplayStep{result: exchange.SubmitResult{}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
			chain := newPayoutReplayChain(fx.chain, map[string][]payoutReplayStep{
				"0xrecipient": {{result: exchange.SubmitResult{}}, test.replayStep},
			})
			chain.store = fx.store
			fx.orchestrator.chain = chain

			err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
			if err == nil || !IsFatal(err) {
				t.Fatalf("Run() error = %v, want terminal blocked result", err)
			}
			assertOneExactPayoutReplay(t, chain, "0xrecipient")
			if fx.store.current == nil ||
				testTarget(t, fx.store.current, PayoutKindPointsRebate).Phase != PhaseBlocked ||
				len(fx.repo.completedIDs) != 0 {
				t.Fatalf("current=%#v completed=%v", fx.store.current, fx.repo.completedIDs)
			}
		})
	}
}

func TestExactReplayFailureKindsAllEndInTerminalBlocked(t *testing.T) {
	tests := []struct {
		name   string
		result exchange.SubmitResult
		err    error
	}{
		{name: "network timeout", err: context.DeadlineExceeded},
		{name: "unknown response"},
		{name: "explicit error response", result: exchange.SubmitResult{
			Rejected: true, Response: json.RawMessage(`{"status":"err","response":"temporary rejection"}`),
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
			chain := newPayoutReplayChain(fx.chain, map[string][]payoutReplayStep{
				"0xrecipient": {
					{result: exchange.SubmitResult{}},
					{result: test.result, err: test.err},
				},
			})
			fx.orchestrator.chain = chain

			err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
			if err == nil || !IsFatal(err) {
				t.Fatalf("Run() error = %v, want fatal blocked result", err)
			}
			assertOneExactPayoutReplay(t, chain, "0xrecipient")
			target := testTarget(t, fx.store.current, PayoutKindPointsRebate)
			if target.Phase != PhaseBlocked || target.Payout.SubmitAttempts != 2 ||
				len(fx.repo.completedIDs) != 0 {
				t.Fatalf("target=%#v completed=%v", target, fx.repo.completedIDs)
			}

			beforeRecovery := len(settlementPayoutActions(chain.submittedActions))
			if recoverErr := fx.orchestrator.Recover(context.Background()); recoverErr == nil || !IsFatal(recoverErr) {
				t.Fatalf("Recover() error = %v, want terminal blocked result", recoverErr)
			}
			if afterRecovery := len(settlementPayoutActions(chain.submittedActions)); afterRecovery != beforeRecovery {
				t.Fatalf("payout submissions after recovery = %d, want %d", afterRecovery, beforeRecovery)
			}
		})
	}
}

func TestPayoutSubmittingRecoveryReplaysOnlyThePersistedRequest(t *testing.T) {
	fx := newFixture(t)
	state := fx.positiveState(t, PhasePayoutSubmitting)
	target := testTarget(t, &state, PayoutKindPointsRebate)
	target.Payout = &PayoutJournal{
		Prepared: validPayout(t, state, 1801), TotalBefore: "2", SubmitAttempts: 1,
	}
	want := clonePrepared(target.Payout.Prepared)
	fx.store.current = &state
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	chain := newPayoutReplayChain(fx.chain, map[string][]payoutReplayStep{
		"0xrecipient": {{result: exchange.SubmitResult{Accepted: true}, apply: true}},
	})
	chain.store = fx.store
	fx.orchestrator.chain = chain

	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	actions := settlementPayoutActions(chain.submittedActions)
	if len(actions) != 1 || !reflect.DeepEqual(actions[0], want) {
		t.Fatalf("recovery replay actions = %#v, want exact %#v", actions, want)
	}
	if got := chain.prepareCalls["0xrecipient"]; got != 0 {
		t.Fatalf("recovery PrepareSpotSend calls = %d, want 0", got)
	}
	if !chain.durableReplayAtSubmission["0xrecipient"] {
		t.Fatalf("recovery replay intent was not durable before Submit: %#v", fx.store.saved)
	}
}

func TestPayoutReplayingRecoveryNeverSubmitsAgain(t *testing.T) {
	fx := newFixture(t)
	state := fx.positiveState(t, PhasePayoutReplaying)
	target := testTarget(t, &state, PayoutKindPointsRebate)
	target.Payout = &PayoutJournal{
		Prepared: validPayout(t, state, 1802), TotalBefore: "2", SubmitAttempts: 2,
	}
	fx.store.current = &state
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	chain := newPayoutReplayChain(fx.chain, map[string][]payoutReplayStep{})
	fx.orchestrator.chain = chain

	err := fx.orchestrator.Recover(context.Background())
	if err == nil || !IsFatal(err) {
		t.Fatalf("Recover() error = %v, want blocked result", err)
	}
	if len(settlementPayoutActions(chain.submittedActions)) != 0 ||
		len(chain.prepareCalls) != 0 {
		t.Fatalf("replaying recovery submitted=%#v prepared=%#v",
			chain.submittedActions, chain.prepareCalls)
	}
	if fx.store.current == nil ||
		testTarget(t, fx.store.current, PayoutKindPointsRebate).Phase != PhaseBlocked {
		t.Fatalf("current = %#v", fx.store.current)
	}
}

func TestAmbiguousPayoutRejectsAnyOtherSettlementDecrease(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("2.5")
	fx.chain.balanceErrors["0xrecipient"] = errors.New("recipient balance unavailable")
	chain := newPayoutReplayChain(fx.chain, map[string][]payoutReplayStep{
		"0xrecipient": {
			// 1.5 payout + 0.5 unexplained decrease is neither accepted boundary.
			{result: exchange.SubmitResult{}, apply: true, activationFee: decimal.RequireFromString("0.5")},
			{result: duplicateNonceResult(1786010960000)},
		},
	})
	fx.orchestrator.chain = chain

	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || !IsFatal(err) {
		t.Fatalf("Run() error = %v, want blocked result", err)
	}
	// A changed but unrecognised balance is not a stable pre-submit boundary,
	// so replaying could be unsafe. Only the initial request may be submitted.
	if actions := settlementPayoutActions(chain.submittedActions); len(actions) != 1 {
		t.Fatalf("payout submissions = %d, want no replay: %#v", len(actions), actions)
	}
	if got := chain.prepareCalls["0xrecipient"]; got != 1 {
		t.Fatalf("PrepareSpotSend calls = %d, want 1", got)
	}
	if len(fx.repo.completedIDs) != 0 || fx.store.current == nil ||
		testTarget(t, fx.store.current, PayoutKindPointsRebate).Phase != PhaseBlocked {
		t.Fatalf("completed=%v current=%#v", fx.repo.completedIDs, fx.store.current)
	}
}

func TestAmbiguousPayoutDoesNotReplayWithoutACompleteSettlementObservationWindow(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	// The two successful reads establish the pre-submit balance. Every bounded
	// post-submit query then fails, so unchanged state was never proven.
	fx.chain.balanceErrorSequence["0xsettlement"] = make(
		[]error, 2, 2+payoutBalanceQueryAttempts,
	)
	for range payoutBalanceQueryAttempts {
		fx.chain.balanceErrorSequence["0xsettlement"] = append(
			fx.chain.balanceErrorSequence["0xsettlement"],
			errors.New("temporary settlement info failure"),
		)
	}
	chain := newPayoutReplayChain(fx.chain, map[string][]payoutReplayStep{
		"0xrecipient": {{result: exchange.SubmitResult{}}},
	})
	fx.orchestrator.chain = chain

	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || !IsFatal(err) {
		t.Fatalf("Run() error = %v, want blocked result", err)
	}
	if actions := settlementPayoutActions(chain.submittedActions); len(actions) != 1 {
		t.Fatalf("payout submissions = %d, want no replay: %#v", len(actions), actions)
	}
	if got := chain.prepareCalls["0xrecipient"]; got != 1 {
		t.Fatalf("PrepareSpotSend calls = %d, want 1", got)
	}
	if fx.store.current == nil ||
		testTarget(t, fx.store.current, PayoutKindPointsRebate).Phase != PhaseBlocked {
		t.Fatalf("current = %#v", fx.store.current)
	}
}

func TestDualPayoutCarriesObservedActivationFeesIntoTheNextSettlementBoundary(t *testing.T) {
	fx := newFixture(t)
	fx.referralRepo.records = []Record{{ID: 9, PeriodStartAt: 30, Amount: "0.25"}}
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("3.75")
	fx.chain.balanceErrors["0xrecipient"] = errors.New("recipient balance unavailable")
	fx.chain.balanceErrors["0xreferral"] = errors.New("recipient balance unavailable")
	chain := newPayoutReplayChain(fx.chain, map[string][]payoutReplayStep{
		"0xrecipient": {{
			result: exchange.SubmitResult{}, apply: true, activationFee: decimal.NewFromInt(1),
		}},
		"0xreferral": {{
			result: exchange.SubmitResult{}, apply: true, activationFee: decimal.NewFromInt(1),
		}},
	})
	fx.orchestrator.chain = chain

	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}

	actions := settlementPayoutActions(fx.chain.submittedActions)
	if len(actions) != 2 || actions[0].Destination != "0xrecipient" ||
		actions[1].Destination != "0xreferral" {
		t.Fatalf("payout actions = %#v", actions)
	}
	if chain.prepareCalls["0xrecipient"] != 1 || chain.prepareCalls["0xreferral"] != 1 {
		t.Fatalf("prepare calls = %#v", chain.prepareCalls)
	}
	if fx.chain.balanceCalls["0xrecipient"] != 0 || fx.chain.balanceCalls["0xreferral"] != 0 {
		t.Fatalf("recipient balance calls: points=%d referral=%d",
			fx.chain.balanceCalls["0xrecipient"], fx.chain.balanceCalls["0xreferral"])
	}
	if !equalUint64(fx.repo.completedIDs, []uint64{1, 2}) ||
		!equalUint64(fx.referralRepo.completedIDs, []uint64{9}) {
		t.Fatalf("points completed=%v referral completed=%v",
			fx.repo.completedIDs, fx.referralRepo.completedIDs)
	}
	report := onlyReport(t, fx.notifier)
	if report.target(PayoutKindPointsRebate).Payout.Status != ReportStepSuccess ||
		report.target(PayoutKindReferral).Payout.Status != ReportStepSuccess ||
		!report.SettlementBalance.Observed || report.SettlementBalance.Total != "0" {
		t.Fatalf("dual payout report = %#v", report)
	}
}

type payoutReplayStep struct {
	result        exchange.SubmitResult
	err           error
	apply         bool
	activationFee decimal.Decimal
}

// payoutReplayChain models transport outcomes independently from whether the
// exchange applied an action. Applied payouts intentionally do not credit the
// recipient, modelling an immediate onward transfer from that account.
type payoutReplayChain struct {
	*fakeChain
	steps                          map[string][]payoutReplayStep
	attempts                       map[string]int
	prepareCalls                   map[string]int
	store                          *fakeStore
	durableReplayAtSubmission      map[string]bool
	hideSettlementUntilSubmissions int
	hiddenSettlement               info.SpotBalanceAmounts
}

func newPayoutReplayChain(
	base *fakeChain,
	steps map[string][]payoutReplayStep,
) *payoutReplayChain {
	return &payoutReplayChain{
		fakeChain: base,
		steps:     steps, attempts: make(map[string]int), prepareCalls: make(map[string]int),
		durableReplayAtSubmission: make(map[string]bool),
	}
}

func (c *payoutReplayChain) PrepareSpotSend(
	address, destination string,
	token info.Token,
	amount decimal.Decimal,
	nonce uint64,
) (exchange.PreparedAction, error) {
	if address == "0xsettlement" {
		c.mu.Lock()
		c.prepareCalls[destination]++
		c.mu.Unlock()
	}
	return c.fakeChain.PrepareSpotSend(address, destination, token, amount, nonce)
}

func (c *payoutReplayChain) Submit(
	ctx context.Context,
	action exchange.PreparedAction,
) (exchange.SubmitResult, error) {
	if action.Kind != "spotSend" || action.Signer != "0xsettlement" {
		return c.fakeChain.Submit(ctx, action)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, "submit:"+action.Kind+":"+action.Signer)
	c.submittedActions = append(c.submittedActions, clonePrepared(action))
	index := c.attempts[action.Destination]
	c.attempts[action.Destination] = index + 1
	if c.store != nil {
		c.durableReplayAtSubmission[action.Destination] = replayIntentIsDurable(c.store, action)
	}
	steps := c.steps[action.Destination]
	if index >= len(steps) {
		return exchange.SubmitResult{}, fmt.Errorf(
			"unexpected payout submission %d for %s", index+1, action.Destination,
		)
	}
	step := steps[index]
	if step.apply {
		amount := decimal.RequireFromString(action.Amount).Add(step.activationFee)
		c.balances[action.Signer] = c.balances[action.Signer].Sub(amount)
	}
	return step.result, step.err
}

func (c *payoutReplayChain) SpotBalance(
	ctx context.Context,
	address string,
	token info.Token,
) (info.SpotBalanceAmounts, error) {
	if address == "0xsettlement" && c.hideSettlementUntilSubmissions > 0 {
		c.mu.Lock()
		submissions := 0
		for _, count := range c.attempts {
			submissions += count
		}
		if submissions < c.hideSettlementUntilSubmissions {
			c.balanceCalls[address]++
			balance := c.hiddenSettlement
			c.mu.Unlock()
			return balance, nil
		}
		c.mu.Unlock()
	}
	return c.fakeChain.SpotBalance(ctx, address, token)
}

func duplicateNonceResult(nonce uint64) exchange.SubmitResult {
	return exchange.SubmitResult{
		Rejected: true,
		Response: json.RawMessage(fmt.Sprintf(
			`{"status":"err","response":"Invalid nonce: duplicate nonce %d"}`,
			nonce,
		)),
	}
}

func assertOneExactPayoutReplay(
	t *testing.T,
	chain *payoutReplayChain,
	destination string,
) {
	t.Helper()
	actions := settlementPayoutActions(chain.submittedActions)
	if len(actions) != 2 {
		t.Fatalf("settlement payout submissions = %d, want 2: %#v", len(actions), actions)
	}
	if actions[0].Destination != destination || actions[1].Destination != destination {
		t.Fatalf("payout destinations = %q, %q, want %q",
			actions[0].Destination, actions[1].Destination, destination)
	}
	if !reflect.DeepEqual(actions[0], actions[1]) {
		t.Fatalf("replay changed persisted request:\nfirst  = %#v\nreplay = %#v", actions[0], actions[1])
	}
	if chain.store != nil && !chain.durableReplayAtSubmission[destination] {
		t.Fatalf("replay for %s began before two durable replaying snapshots", destination)
	}
}

func replayIntentIsDurable(store *fakeStore, action exchange.PreparedAction) bool {
	return lastTwoReplaySnapshotsMatch(store, action)
}

func lastTwoReplaySnapshotsMatch(store *fakeStore, action exchange.PreparedAction) bool {
	if store == nil || len(store.saved) < 2 {
		return false
	}
	for _, snapshot := range store.saved[len(store.saved)-2:] {
		target, _, err := snapshot.activePayoutTarget()
		if err != nil || target == nil || target.Phase != PhasePayoutReplaying ||
			target.Payout == nil || target.Payout.SubmitAttempts != 2 ||
			!reflect.DeepEqual(target.Payout.Prepared, action) {
			return false
		}
	}
	return true
}
