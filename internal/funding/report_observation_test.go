package funding

import (
	"context"
	"testing"

	"builder-code-bot/internal/hyperliquid/info"

	"github.com/shopspring/decimal"
)

func TestPostPayoutBalanceBoundariesIncludeNewAddressActivationFee(t *testing.T) {
	t.Parallel()
	report := RunReport{Payouts: []PayoutTargetReport{{
		Payout: PayoutReport{
			Status: ReportStepSuccess, Amount: "1.5", SettlementTotalBefore: "3",
		},
	}}}

	boundaries, required, err := postPayoutBalanceBoundaries(&report)
	if err != nil {
		t.Fatal(err)
	}
	if !required || len(boundaries) != 2 ||
		!boundaries[0].Equal(decimal.RequireFromString("1.5")) ||
		!boundaries[1].Equal(decimal.RequireFromString("0.5")) {
		t.Fatalf("boundaries = %v, required = %t", boundaries, required)
	}
	if !matchesAnyBalanceBoundary(decimal.RequireFromString("1.5"), boundaries) ||
		!matchesAnyBalanceBoundary(decimal.RequireFromString("0.5"), boundaries) ||
		matchesAnyBalanceBoundary(decimal.RequireFromString("1.4"), boundaries) {
		t.Fatalf("unexpected boundary matching for %v", boundaries)
	}
}

func TestPostPayoutBalanceBoundariesNeverAdmitNegativeActivationFeeBoundary(t *testing.T) {
	t.Parallel()
	report := RunReport{Payouts: []PayoutTargetReport{{
		Payout: PayoutReport{
			Status: ReportStepSuccess, Amount: "1.5", SettlementTotalBefore: "2",
		},
	}}}

	boundaries, required, err := postPayoutBalanceBoundaries(&report)
	if err != nil {
		t.Fatal(err)
	}
	if !required || len(boundaries) != 1 ||
		!boundaries[0].Equal(decimal.RequireFromString("0.5")) {
		t.Fatalf("boundaries = %v, required = %t", boundaries, required)
	}
}

func TestFinalBalanceObservationRequiresSameBoundaryTwice(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	report := newRunReport(
		fx.clock.now, TriggerUTC, ReportExecution{}, "mainnet",
		fx.orchestrator.builders, fx.orchestrator.builderNames,
		"0xsettlement", "0xrecipient",
	)
	report.token = cloneToken(&fx.chain.token)
	report.target(PayoutKindReferral).Payout = PayoutReport{
		Status: ReportStepSuccess, Amount: "1", SettlementTotalBefore: "3",
	}
	fx.chain.balanceSequence["0xsettlement"] = []info.SpotBalanceAmounts{
		{Total: decimal.NewFromInt(2), Available: decimal.RequireFromString("0.5")},
		{Total: decimal.NewFromInt(1), Available: decimal.RequireFromString("0.5")},
		{Total: decimal.NewFromInt(1), Available: decimal.RequireFromString("0.5")},
	}

	fx.orchestrator.observeSettlementFinalBalance(context.Background(), &report)

	if !report.SettlementBalance.Observed || report.SettlementBalance.Total != "1" ||
		fx.chain.balanceCalls["0xsettlement"] != 3 {
		t.Fatalf("settlement balance = %#v, calls = %d",
			report.SettlementBalance, fx.chain.balanceCalls["0xsettlement"])
	}
}

func TestRunReportSyncStateRecordsExactReplayAndFinalBlocked(t *testing.T) {
	t.Parallel()
	state := RunState{Targets: []PayoutTargetState{
		{
			Kind: PayoutKindReferral, Phase: PhaseBlocked,
			BlockedReason: uncertainPayoutBlockedReasonPrefix + " after exact replay",
			Manifest:      Manifest{PayoutTotal: "3.25", Settlement: "0xsettlement", Recipient: "0xreferral"},
			Payout: &PayoutJournal{
				TotalBefore: "10", SubmitAttempts: 2,
			},
		},
	}}
	report := newRunReport(
		fixedClock{}.Now(), TriggerUTC, ReportExecution{}, "mainnet",
		nil, nil, "0xsettlement", "0xreferral",
	)

	report.syncState(&state)

	payout := report.target(PayoutKindReferral).Payout
	if payout.SubmitAttempts != 2 || !payout.Blocked || payout.SettlementTotalBefore != "10" {
		t.Fatalf("referral payout report = %#v", payout)
	}
}
