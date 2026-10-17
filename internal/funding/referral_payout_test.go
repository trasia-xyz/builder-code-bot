package funding

import (
	"context"
	"reflect"
	"testing"
	"time"

	"builder-code-bot/internal/hyperliquid/exchange"
	"builder-code-bot/internal/hyperliquid/info"

	"github.com/shopspring/decimal"
)

type postPayoutObservationProbe struct {
	awaitingBalance     bool
	delayed             bool
	firstBalanceRead    bool
	readOccurredTooSoon bool
}

type postPayoutDelayChain struct {
	*fakeChain
	probe *postPayoutObservationProbe
}

func (c *postPayoutDelayChain) Submit(
	ctx context.Context,
	action exchange.PreparedAction,
) (exchange.SubmitResult, error) {
	result, err := c.fakeChain.Submit(ctx, action)
	if err == nil && result.Accepted && action.Kind == "spotSend" &&
		action.Signer == "0xsettlement" && action.Destination == "0xrecipient" {
		c.probe.awaitingBalance = true
	}
	return result, err
}

func (c *postPayoutDelayChain) SpotBalance(
	ctx context.Context,
	address string,
	token info.Token,
) (info.SpotBalanceAmounts, error) {
	if address == "0xsettlement" && c.probe.awaitingBalance && !c.probe.firstBalanceRead {
		c.probe.firstBalanceRead = true
		c.probe.readOccurredTooSoon = !c.probe.delayed
		c.probe.awaitingBalance = false
	}
	return c.fakeChain.SpotBalance(ctx, address, token)
}

type postPayoutDelaySleeper struct {
	*fakeSleeper
	probe *postPayoutObservationProbe
}

func (s *postPayoutDelaySleeper) Sleep(ctx context.Context, delay time.Duration) error {
	if s.probe.awaitingBalance && delay >= time.Second {
		s.probe.delayed = true
	}
	return s.fakeSleeper.Sleep(ctx, delay)
}

func TestReferralPayoutCompletesRoundedSnapshot(t *testing.T) {
	fx := newFixture(t)
	fx.repo.records = []Record{
		{ID: 9, PeriodStartAt: 30, Amount: "0.250000000000000001"},
	}
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)

	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}

	actions := settlementPayoutActions(fx.chain.submittedActions)
	if len(actions) != 1 || actions[0].Destination != "0xrecipient" ||
		actions[0].Amount != "0.250001" {
		t.Fatalf("payout actions = %#v", actions)
	}
	if !equalUint64(fx.repo.completedIDs, []uint64{9}) {
		t.Fatalf("referral completed = %v", fx.repo.completedIDs)
	}
	if len(fx.store.archived.Targets) != 1 {
		t.Fatalf("archived targets = %#v", fx.store.archived.Targets)
	}
	referral := testTarget(t, &fx.store.archived, PayoutKindReferral)
	if referral.Phase != PhaseCompleted || referral.Payout == nil ||
		referral.Payout.TotalBefore != "2" {
		t.Fatalf("archived referral = %#v", referral)
	}
	report := onlyReport(t, fx.notifier)
	if report.PayoutTotal != "0.250001" || report.RecordCount != 1 || len(report.Payouts) != 1 ||
		report.target(PayoutKindReferral).Payout.Status != ReportStepSuccess ||
		report.target(PayoutKindReferral).DatabaseStatus != ReportStepSuccess ||
		!report.SettlementBalance.Observed || report.SettlementBalance.Total != "1.749999" {
		t.Fatalf("referral payout report = %#v", report)
	}
	if stage := report.stage("finalize"); stage == nil || stage.Status != ReportStepSuccess {
		t.Fatalf("finalize stage = %#v", stage)
	}
}

func TestReferralPayoutWaitsBeforeFirstPostTransferBalanceRead(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	probe := &postPayoutObservationProbe{}
	fx.orchestrator.chain = &postPayoutDelayChain{fakeChain: fx.chain, probe: probe}
	fx.orchestrator.sleeper = &postPayoutDelaySleeper{fakeSleeper: fx.sleeper, probe: probe}

	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	if !probe.firstBalanceRead {
		t.Fatal("no settlement balance read was observed after the referral payout")
	}
	if !probe.delayed || probe.readOccurredTooSoon {
		t.Fatalf("first post-payout read delayed=%t tooSoon=%t, want delay before read",
			probe.delayed, probe.readOccurredTooSoon)
	}
}

func TestReferralPayoutRequiresAvailableBeforeSubmission(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	fx.chain.holds["0xsettlement"] = decimal.NewFromInt(1)

	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || IsFatal(err) {
		t.Fatalf("Run() error = %v, want ordinary insufficient-balance error", err)
	}
	if actions := settlementPayoutActions(fx.chain.submittedActions); len(actions) != 0 {
		t.Fatalf("submitted payouts = %#v", actions)
	}
	if len(fx.repo.completedIDs) != 0 {
		t.Fatalf("referral completed = %v", fx.repo.completedIDs)
	}
	if fx.store.current == nil ||
		testTarget(t, fx.store.current, PayoutKindReferral).Phase != PhasePrepared {
		t.Fatalf("current = %#v", fx.store.current)
	}
}

func TestInvalidReferralSnapshotDoesNotPublishPayoutTotals(t *testing.T) {
	fx := newFixture(t)
	fx.repo.records = []Record{{ID: 9, PeriodStartAt: 30, Amount: "-0.01"}}
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(10)

	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || !IsFatal(err) {
		t.Fatalf("Run() error = %v, want fatal validation error", err)
	}
	if actions := settlementPayoutActions(fx.chain.submittedActions); len(actions) != 0 {
		t.Fatalf("submitted payouts = %#v", actions)
	}
	if fx.repo.listCalls != 1 || len(fx.repo.completedIDs) != 0 {
		t.Fatalf("referral list/complete = %d/%v", fx.repo.listCalls, fx.repo.completedIDs)
	}
	report := onlyReport(t, fx.notifier)
	if report.PayoutTotal != "" || report.RawTotal != "" ||
		report.DailyConsolidation.CoversPayout || report.DailyConsolidation.BelowPayout ||
		report.stage("finalize").Status != ReportStepSuccess {
		t.Fatalf("validation failure reconciled invalid totals: %#v", report)
	}
}

func TestBlockedReferralRecoveryRemainsTerminalWithoutResubmitting(t *testing.T) {
	fx := newFixture(t)
	fx.repo.records = []Record{{ID: 9, PeriodStartAt: 30, Amount: "0.25"}}
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	fx.chain.payoutResults = map[string][]exchange.SubmitResult{
		"0xrecipient": {{}, {}},
	}

	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || !IsFatal(err) {
		t.Fatalf("Run() error = %v, want blocked referral payout", err)
	}
	if actions := settlementPayoutActions(fx.chain.submittedActions); len(actions) != 2 {
		t.Fatalf("submitted payouts = %#v, want two identical referral attempts", actions)
	} else if !reflect.DeepEqual(actions[0], actions[1]) {
		t.Fatalf("referral replay changed request: first=%#v replay=%#v", actions[0], actions[1])
	}
	if fx.store.current == nil ||
		testTarget(t, fx.store.current, PayoutKindReferral).Phase != PhaseBlocked ||
		!isUncertainPayoutBlock(testTarget(t, fx.store.current, PayoutKindReferral)) {
		t.Fatalf("blocked current = %#v", fx.store.current)
	}
	if len(fx.repo.completedIDs) != 0 {
		t.Fatalf("referral completed = %v", fx.repo.completedIDs)
	}
	blockedReport := onlyReport(t, fx.notifier)
	if stage := blockedReport.stage("finalize"); stage == nil ||
		stage.Status != ReportStepSuccess {
		t.Fatalf("blocked finalize stage = %#v", stage)
	}

	// Once the bounded replay and observation windows end, recovery must not
	// turn a later balance change into success or submit the payout again.
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("1.75")
	fx.chain.balances["0xrecipient"] = decimal.RequireFromString("0.25")
	fx.notifier.reports = nil
	err = fx.orchestrator.Recover(context.Background())
	if err == nil || !IsFatal(err) {
		t.Fatalf("Recover() error = %v, want terminal blocked result", err)
	}
	if actions := settlementPayoutActions(fx.chain.submittedActions); len(actions) != 2 {
		t.Fatalf("recovery resubmitted payout: %#v", actions)
	}
	if fx.store.current == nil || len(fx.repo.completedIDs) != 0 {
		t.Fatalf("recovered current=%#v referral completed=%v", fx.store.current, fx.repo.completedIDs)
	}
	report := onlyReport(t, fx.notifier)
	if report.target(PayoutKindReferral).Payout.Status != ReportStepFailed || report.Outcome != "blocked" {
		t.Fatalf("recovery report = %#v", report)
	}
}

func TestRejectedReferralCanRetryPendingSnapshot(t *testing.T) {
	fx := newFixture(t)
	fx.repo.records = []Record{{ID: 9, PeriodStartAt: 30, Amount: "0.25"}}
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	fx.chain.payoutResults = map[string][]exchange.SubmitResult{
		"0xrecipient": {{Rejected: true}},
	}

	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || !IsFatal(err) || fx.store.current != nil || fx.store.archiveResult != "rejected" {
		t.Fatalf("first run error=%v current=%#v archive=%q", err, fx.store.current, fx.store.archiveResult)
	}
	if actions := settlementPayoutActions(fx.chain.submittedActions); len(actions) != 1 ||
		actions[0].Destination != "0xrecipient" || len(fx.repo.completedIDs) != 0 {
		t.Fatalf("first run payout actions=%#v completed=%v", actions, fx.repo.completedIDs)
	}

	fx.notifier.reports = nil
	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	actions := settlementPayoutActions(fx.chain.submittedActions)
	if len(actions) != 2 || actions[1].Destination != "0xrecipient" ||
		actions[1].Amount != "0.25" || actions[0].Nonce == actions[1].Nonce {
		t.Fatalf("retry payout actions = %#v", actions)
	}
	if !equalUint64(fx.repo.completedIDs, []uint64{9}) {
		t.Fatalf("referral completed = %v", fx.repo.completedIDs)
	}
}

func TestLegacyPayoutStateIsRejectedBeforeWrites(t *testing.T) {
	for _, phase := range []Phase{PhasePrepared, PhasePayoutSubmitting, PhaseCompleted} {
		for _, recoverOnly := range []bool{false, true} {
			name := string(phase) + "/run"
			if recoverOnly {
				name = string(phase) + "/recover"
			}
			t.Run(name, func(t *testing.T) {
				fx := newFixture(t)
				state := fx.positiveState(t, PhasePrepared)
				legacy := state.Targets[0]
				legacy.Kind = PayoutKind("points_rebate")
				legacy.Phase = phase
				state.Targets = append([]PayoutTargetState{legacy}, state.Targets...)
				fx.store.current = &state
				before := cloneState(&state)
				fx.chain.balances["0xsettlement"] = decimal.NewFromInt(10)

				var err error
				if recoverOnly {
					err = fx.orchestrator.Recover(context.Background())
				} else {
					err = fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
				}
				if err == nil || !IsFatal(err) {
					t.Fatalf("legacy state error = %v, want fatal validation error", err)
				}
				if len(fx.chain.events) != 0 || fx.repo.listCalls != 0 || len(fx.repo.completedIDs) != 0 ||
					len(fx.store.saved) != 0 || fx.store.archiveResult != "" ||
					!reflect.DeepEqual(fx.store.current, before) {
					t.Fatalf("legacy state was mutated: events=%v list=%d completed=%v saved=%d archive=%q current=%#v",
						fx.chain.events, fx.repo.listCalls, fx.repo.completedIDs, len(fx.store.saved),
						fx.store.archiveResult, fx.store.current)
				}
			})
		}
	}
}

func settlementPayoutActions(actions []exchange.PreparedAction) []exchange.PreparedAction {
	result := make([]exchange.PreparedAction, 0, len(actions))
	for _, action := range actions {
		if action.Kind == "spotSend" && action.Signer == "0xsettlement" {
			result = append(result, action)
		}
	}
	return result
}
