package funding

import (
	"context"
	"reflect"
	"testing"

	"builder-code-bot/internal/hyperliquid/exchange"
	"builder-code-bot/internal/hyperliquid/info"

	"github.com/shopspring/decimal"
)

func TestDualPayoutExecutesPointsThenReferralAndCompletesBothTargets(t *testing.T) {
	fx := newFixture(t)
	fx.referralRepo.records = []Record{
		{ID: 1, PeriodStartAt: 30, Amount: "0.250000000000000001"},
	}
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)

	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}

	actions := settlementPayoutActions(fx.chain.submittedActions)
	if len(actions) != 2 {
		t.Fatalf("settlement payout actions = %#v, want 2", actions)
	}
	if actions[0].Destination != "0xrecipient" || actions[0].Amount != "1.5" ||
		actions[1].Destination != "0xreferral" || actions[1].Amount != "0.250001" {
		t.Fatalf("payout order/actions = %#v", actions)
	}
	if !equalUint64(fx.repo.completedIDs, []uint64{1, 2}) ||
		!equalUint64(fx.referralRepo.completedIDs, []uint64{1}) {
		t.Fatalf("points completed=%v referral completed=%v",
			fx.repo.completedIDs, fx.referralRepo.completedIDs)
	}
	points := testTarget(t, &fx.store.archived, PayoutKindPointsRebate)
	referral := testTarget(t, &fx.store.archived, PayoutKindReferral)
	if points.Phase != PhaseCompleted || referral.Phase != PhaseCompleted ||
		points.Payout == nil || referral.Payout == nil ||
		points.Payout.TotalBefore != "2" || referral.Payout.TotalBefore != "0.5" {
		t.Fatalf("archived targets = %#v", fx.store.archived.Targets)
	}
	report := onlyReport(t, fx.notifier)
	if report.PayoutTotal != "1.750001" || report.RecordCount != 3 ||
		report.target(PayoutKindPointsRebate).Payout.Status != ReportStepSuccess ||
		report.target(PayoutKindReferral).Payout.Status != ReportStepSuccess ||
		report.target(PayoutKindPointsRebate).DatabaseStatus != ReportStepSuccess ||
		report.target(PayoutKindReferral).DatabaseStatus != ReportStepSuccess ||
		!report.SettlementBalance.Observed || report.SettlementBalance.Total != "0.249999" {
		t.Fatalf("dual payout report = %#v", report)
	}
	if stage := report.stage("finalize"); stage == nil || stage.Status != ReportStepSuccess {
		t.Fatalf("finalize stage = %#v", stage)
	}
}

func TestDualPayoutRequiresCombinedAvailableBeforeFirstSubmission(t *testing.T) {
	fx := newFixture(t)
	fx.referralRepo.records = []Record{{ID: 9, PeriodStartAt: 30, Amount: "1"}}
	// Points alone can be funded, but the confirmed policy requires enough for
	// both independently rounded payouts before either external send.
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)

	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || IsFatal(err) {
		t.Fatalf("Run() error = %v, want ordinary combined-balance error", err)
	}
	if actions := settlementPayoutActions(fx.chain.submittedActions); len(actions) != 0 {
		t.Fatalf("submitted payouts = %#v", actions)
	}
	if len(fx.repo.completedIDs) != 0 || len(fx.referralRepo.completedIDs) != 0 {
		t.Fatalf("points completed=%v referral completed=%v",
			fx.repo.completedIDs, fx.referralRepo.completedIDs)
	}
	if fx.store.current == nil ||
		testTarget(t, fx.store.current, PayoutKindPointsRebate).Phase != PhasePrepared ||
		testTarget(t, fx.store.current, PayoutKindReferral).Phase != PhasePrepared {
		t.Fatalf("current = %#v", fx.store.current)
	}
}

func TestReferralWaitsForExactPointsPostPayoutBoundary(t *testing.T) {
	fx := newFixture(t)
	fx.referralRepo.records = []Record{{ID: 9, PeriodStartAt: 30, Amount: "0.25"}}
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	prePoints := info.SpotBalanceAmounts{
		Total: decimal.NewFromInt(2), Available: decimal.NewFromInt(2),
	}
	// The first two samples fund the aggregate preflight. The next five are
	// stale pre-Points snapshots and must never become Referral.TotalBefore.
	fx.chain.balanceSequence["0xsettlement"] = []info.SpotBalanceAmounts{
		prePoints, prePoints,
		prePoints, prePoints, prePoints, prePoints, prePoints,
	}

	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || IsFatal(err) {
		t.Fatalf("Run() error = %v, want ordinary cross-payout visibility error", err)
	}
	actions := settlementPayoutActions(fx.chain.submittedActions)
	if len(actions) != 1 || actions[0].Destination != "0xrecipient" {
		t.Fatalf("submitted payouts = %#v", actions)
	}
	if !equalUint64(fx.repo.completedIDs, []uint64{1, 2}) ||
		len(fx.referralRepo.completedIDs) != 0 {
		t.Fatalf("points completed=%v referral completed=%v",
			fx.repo.completedIDs, fx.referralRepo.completedIDs)
	}
	if fx.store.current == nil ||
		testTarget(t, fx.store.current, PayoutKindPointsRebate).Phase != PhaseCompleted ||
		testTarget(t, fx.store.current, PayoutKindReferral).Phase != PhasePrepared ||
		testTarget(t, fx.store.current, PayoutKindReferral).Payout != nil {
		t.Fatalf("current = %#v", fx.store.current)
	}

	fx.notifier.reports = nil
	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	actions = settlementPayoutActions(fx.chain.submittedActions)
	if len(actions) != 2 || actions[1].Destination != "0xreferral" ||
		actions[1].Amount != "0.25" {
		t.Fatalf("retry payout actions = %#v", actions)
	}
}

func TestInvalidReferralSnapshotBlocksPointsPayout(t *testing.T) {
	fx := newFixture(t)
	fx.referralRepo.records = []Record{{ID: 9, PeriodStartAt: 30, Amount: "-0.01"}}
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
	if fx.repo.listCalls != 1 || fx.referralRepo.listCalls != 1 ||
		len(fx.repo.completedIDs) != 0 || len(fx.referralRepo.completedIDs) != 0 {
		t.Fatalf("points list/complete=%d/%v referral list/complete=%d/%v",
			fx.repo.listCalls, fx.repo.completedIDs,
			fx.referralRepo.listCalls, fx.referralRepo.completedIDs)
	}
	report := onlyReport(t, fx.notifier)
	if report.PayoutTotal != "" || report.RawTotal != "" ||
		report.DailyConsolidation.CoversPayout || report.DailyConsolidation.BelowPayout ||
		report.stage("finalize").Status != ReportStepSuccess {
		t.Fatalf("validation failure reconciled a partial aggregate: %#v", report)
	}
}

func TestBlockedReferralRecoveryRemainsTerminalWithoutResubmitting(t *testing.T) {
	fx := newFixture(t)
	fx.referralRepo.records = []Record{{ID: 9, PeriodStartAt: 30, Amount: "0.25"}}
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	fx.chain.payoutResults = map[string][]exchange.SubmitResult{
		"0xreferral": {{}, {}},
	}

	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || !IsFatal(err) {
		t.Fatalf("Run() error = %v, want blocked Referral payout", err)
	}
	if actions := settlementPayoutActions(fx.chain.submittedActions); len(actions) != 3 {
		t.Fatalf("submitted payouts = %#v, want Points and two identical Referral attempts", actions)
	} else if !reflect.DeepEqual(actions[1], actions[2]) {
		t.Fatalf("Referral replay changed request: first=%#v replay=%#v", actions[1], actions[2])
	}
	if fx.store.current == nil ||
		testTarget(t, fx.store.current, PayoutKindPointsRebate).Phase != PhaseCompleted ||
		testTarget(t, fx.store.current, PayoutKindReferral).Phase != PhaseBlocked ||
		!isUncertainPayoutBlock(testTarget(t, fx.store.current, PayoutKindReferral)) {
		t.Fatalf("blocked current = %#v", fx.store.current)
	}
	if !equalUint64(fx.repo.completedIDs, []uint64{1, 2}) ||
		len(fx.referralRepo.completedIDs) != 0 {
		t.Fatalf("points completed=%v referral completed=%v",
			fx.repo.completedIDs, fx.referralRepo.completedIDs)
	}
	blockedReport := onlyReport(t, fx.notifier)
	if stage := blockedReport.stage("finalize"); stage == nil ||
		stage.Status != ReportStepSuccess {
		t.Fatalf("blocked finalize stage = %#v", stage)
	}

	// Even if the balance later reaches a valid boundary, this task ended after
	// its bounded replay and observation windows. Recovery is terminal and must
	// neither observe into success nor submit again.
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("0.25")
	fx.chain.balances["0xreferral"] = decimal.RequireFromString("0.25")
	fx.notifier.reports = nil
	err = fx.orchestrator.Recover(context.Background())
	if err == nil || !IsFatal(err) {
		t.Fatalf("Recover() error = %v, want terminal blocked result", err)
	}
	if actions := settlementPayoutActions(fx.chain.submittedActions); len(actions) != 3 {
		t.Fatalf("recovery resubmitted payout: %#v", actions)
	}
	if fx.store.current == nil || len(fx.referralRepo.completedIDs) != 0 {
		t.Fatalf("recovered current=%#v referral completed=%v",
			fx.store.current, fx.referralRepo.completedIDs)
	}
	report := onlyReport(t, fx.notifier)
	if report.target(PayoutKindPointsRebate).Payout.Status != ReportStepSuccess ||
		report.target(PayoutKindReferral).Payout.Status != ReportStepFailed ||
		report.Outcome != "blocked" {
		t.Fatalf("recovery report = %#v", report)
	}
}

func TestRejectedReferralRetryNeverResubmitsCompletedPointsPayout(t *testing.T) {
	fx := newFixture(t)
	fx.referralRepo.records = []Record{{ID: 9, PeriodStartAt: 30, Amount: "0.25"}}
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	fx.chain.payoutResults = map[string][]exchange.SubmitResult{
		"0xreferral": {{Rejected: true}},
	}

	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || !IsFatal(err) || fx.store.current != nil ||
		fx.store.archiveResult != "rejected" {
		t.Fatalf("first run error=%v current=%#v archive=%q",
			err, fx.store.current, fx.store.archiveResult)
	}
	if actions := settlementPayoutActions(fx.chain.submittedActions); len(actions) != 2 ||
		actions[0].Destination != "0xrecipient" || actions[1].Destination != "0xreferral" {
		t.Fatalf("first run payout actions = %#v", actions)
	}
	if !equalUint64(fx.repo.completedIDs, []uint64{1, 2}) ||
		len(fx.referralRepo.completedIDs) != 0 {
		t.Fatalf("points completed=%v referral completed=%v",
			fx.repo.completedIDs, fx.referralRepo.completedIDs)
	}

	// The next database snapshot reflects the already-completed Points rows.
	// Only the explicitly rejected Referral records remain pending.
	fx.repo.records = nil
	fx.notifier.reports = nil
	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	actions := settlementPayoutActions(fx.chain.submittedActions)
	if len(actions) != 3 || actions[2].Destination != "0xreferral" ||
		countPayoutDestination(actions, "0xrecipient") != 1 {
		t.Fatalf("retry payout actions = %#v", actions)
	}
	if !equalUint64(fx.referralRepo.completedIDs, []uint64{9}) {
		t.Fatalf("referral completed=%v", fx.referralRepo.completedIDs)
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

func countPayoutDestination(actions []exchange.PreparedAction, destination string) int {
	count := 0
	for _, action := range actions {
		if action.Destination == destination {
			count++
		}
	}
	return count
}
