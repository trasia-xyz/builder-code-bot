package funding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"builder-code-bot/internal/hyperliquid/exchange"
	"builder-code-bot/internal/hyperliquid/info"

	"github.com/shopspring/decimal"
)

func TestDailyConsolidationAndFundingAreSeparatePhases(t *testing.T) {
	fx := newFixture(t)
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !equalStrings(fx.chain.events, []string{
		"submit:claimRewards:0xbuilder",
		"submit:spotSend:0xbuilder",
	}) {
		t.Fatalf("UTC 00:00 chain events = %v", fx.chain.events)
	}
	if fx.repo.listCalls != 0 || len(fx.store.saved) != 0 {
		t.Fatalf("UTC 00:00 touched payout state: list=%d saved=%d", fx.repo.listCalls, len(fx.store.saved))
	}
	record := fx.store.consolidations["2026-07-12"]
	if record.Status != DailyConsolidationCompleted || len(record.Builders) != 1 ||
		!record.Builders[0].ClaimableObserved || record.Builders[0].ClaimableUSDC != "1.5" ||
		record.Builders[0].ClaimUncertain || record.Builders[0].SweepAmount != "1.5" {
		t.Fatalf("daily consolidation = %#v", record)
	}
	claimCalls := fx.chain.claimableCalls["0xbuilder"]
	balanceCalls := fx.chain.balanceCalls["0xbuilder"]

	fx.orchestrator.clock = fixedClock{now: time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)}
	if err := fx.orchestrator.RunScheduled(context.Background(), TriggerUTC, 1, 6); err != nil {
		t.Fatal(err)
	}
	if !equalStrings(fx.chain.events, []string{
		"submit:claimRewards:0xbuilder",
		"submit:spotSend:0xbuilder",
		"submit:spotSend:0xsettlement",
	}) {
		t.Fatalf("full day chain events = %v", fx.chain.events)
	}
	if fx.chain.claimableCalls["0xbuilder"] != claimCalls ||
		fx.chain.balanceCalls["0xbuilder"] != balanceCalls {
		t.Fatal("UTC 01:00 queried or mutated a builder account")
	}
	if !equalUint64(fx.repo.completedIDs, []uint64{1, 2}) || fx.store.archiveResult != "completed" {
		t.Fatalf("completed IDs = %v, archive = %q", fx.repo.completedIDs, fx.store.archiveResult)
	}
	report := onlyReport(t, fx.notifier)
	if report.Status != ReportStatusSuccess ||
		report.target(PayoutKindReferral).Payout.Status != ReportStepSuccess ||
		!report.DailyConsolidation.Observed ||
		report.DailyConsolidation.TotalClaimableUSDC != "1.5" ||
		!report.DailyConsolidation.CoversPayout ||
		report.Builders[0].ClaimableUSDC != "1.5" ||
		report.Builders[0].SweepAmount != "1.5" {
		t.Fatalf("run report = %#v", report)
	}
}

func TestFinalReportWaitsForAcceptedPayoutBalanceVisibility(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(1)
	fx.chain.holds["0xsettlement"] = decimal.RequireFromString("0.25")
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}

	stale := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("2.5"), Available: decimal.RequireFromString("2.25"),
	}
	fx.chain.balanceSequence["0xsettlement"] = []info.SpotBalanceAmounts{
		stale, // Stable UTC 01:00 payout sufficiency boundary.
		stale,
	}
	fx.orchestrator.clock = fixedClock{now: time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)}
	balanceCallsBefore := fx.chain.balanceCalls["0xsettlement"]
	if err := fx.orchestrator.RunScheduled(context.Background(), TriggerUTC, 1, 6); err != nil {
		t.Fatal(err)
	}

	report := onlyReport(t, fx.notifier)
	balance := report.SettlementBalance
	referralPayout := report.target(PayoutKindReferral).Payout
	if referralPayout.Status != ReportStepSuccess || !balance.Observed ||
		balance.QueryFailed || balance.PostPayoutUnconfirmed ||
		balance.Total != "1" || balance.Hold != "0.25" || balance.Available != "0.75" {
		t.Fatalf("final report payout=%#v settlement balance=%#v", referralPayout, balance)
	}
	if calls := fx.chain.balanceCalls["0xsettlement"] - balanceCallsBefore; calls != 4 {
		t.Fatalf("UTC 01:00 settlement balance calls = %d, want two payout checks plus two final observations", calls)
	}
}

func TestFinalReportDoesNotLabelStalePostPayoutBalanceAsEndingBalance(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(1)
	fx.chain.holds["0xsettlement"] = decimal.RequireFromString("0.25")
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}

	stale := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("2.5"), Available: decimal.RequireFromString("2.25"),
	}
	fx.chain.balanceSequence["0xsettlement"] = make(
		[]info.SpotBalanceAmounts,
		settlementBalanceRequiredConfirmations+settlementFinalBalanceObservationAttempts,
	)
	for index := range fx.chain.balanceSequence["0xsettlement"] {
		fx.chain.balanceSequence["0xsettlement"][index] = stale
	}
	fx.orchestrator.clock = fixedClock{now: time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)}
	balanceCallsBefore := fx.chain.balanceCalls["0xsettlement"]
	delaysBefore := countDelay(fx.sleeper.delays, settlementFinalBalanceObservationInterval)
	if err := fx.orchestrator.RunScheduled(context.Background(), TriggerUTC, 1, 6); err != nil {
		t.Fatal(err)
	}

	report := onlyReport(t, fx.notifier)
	balance := report.SettlementBalance
	warning := "Settlement: Payout 后余额未在有限观察内更新，任务结束余额无法确认"
	if report.Status != ReportStatusWarning ||
		report.target(PayoutKindReferral).Payout.Status != ReportStepSuccess ||
		balance.Observed || balance.QueryFailed || !balance.PostPayoutUnconfirmed ||
		balance.Total != "" || balance.Hold != "" || balance.Available != "" ||
		!containsString(report.Warnings, warning) {
		t.Fatalf("final report = %#v", report)
	}
	if calls := fx.chain.balanceCalls["0xsettlement"] - balanceCallsBefore; calls !=
		settlementBalanceRequiredConfirmations+settlementFinalBalanceObservationAttempts {
		t.Fatalf("UTC 01:00 settlement balance calls = %d, want %d payout checks + %d final observations",
			calls, settlementBalanceRequiredConfirmations, settlementFinalBalanceObservationAttempts)
	}
	wantDelays := 1 + settlementFinalBalanceObservationAttempts
	if delays := countDelay(fx.sleeper.delays, settlementFinalBalanceObservationInterval) - delaysBefore; delays != wantDelays {
		t.Fatalf("payout boundary and post-payout observation delays = %d, want %d",
			delays, wantDelays)
	}
}

func TestFinalReportRejectsHistoricalLowerBalanceUntilExpectedTotalIsStable(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(1)
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	prePayout := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("2.5"), Available: decimal.RequireFromString("2.5"),
	}
	historicalLower := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("0.25"), Available: decimal.RequireFromString("0.25"),
	}
	expected := info.SpotBalanceAmounts{Total: decimal.NewFromInt(1), Available: decimal.NewFromInt(1)}
	fx.chain.balanceSequence["0xsettlement"] = []info.SpotBalanceAmounts{
		prePayout, prePayout,
		historicalLower,
		historicalLower,
		expected,
		expected,
	}
	fx.orchestrator.clock = fixedClock{now: time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)}
	if err := fx.orchestrator.RunScheduled(context.Background(), TriggerUTC, 1, 6); err != nil {
		t.Fatal(err)
	}

	balance := onlyReport(t, fx.notifier).SettlementBalance
	if !balance.Observed || balance.PostPayoutUnconfirmed || balance.Total != "1" {
		t.Fatalf("final settlement balance = %#v", balance)
	}
}

func TestFinalReportRequiresStableAvailableAtExpectedTotal(t *testing.T) {
	fx := newFixture(t)
	report := newRunReport(
		fx.clock.now, TriggerUTC, ReportExecution{}, "mainnet",
		fx.orchestrator.builders, fx.orchestrator.builderNames,
		"0xsettlement", "0xrecipient",
	)
	report.token = cloneToken(&fx.chain.token)
	report.target(PayoutKindReferral).Payout = PayoutReport{
		Status: ReportStepSuccess, Amount: "1.5", SettlementTotalBefore: "2.5",
	}
	fx.chain.balanceSequence["0xsettlement"] = []info.SpotBalanceAmounts{
		{Total: decimal.NewFromInt(1), Available: decimal.RequireFromString("0.9")},
		{Total: decimal.NewFromInt(1), Available: decimal.RequireFromString("0.8")},
		{Total: decimal.NewFromInt(1), Available: decimal.RequireFromString("0.8")},
	}

	fx.orchestrator.observeSettlementFinalBalance(context.Background(), &report)

	if !report.SettlementBalance.Observed || report.SettlementBalance.Available != "0.8" ||
		fx.chain.balanceCalls["0xsettlement"] != 3 {
		t.Fatalf("settlement balance = %#v, calls=%d",
			report.SettlementBalance, fx.chain.balanceCalls["0xsettlement"])
	}
}

func TestFinalReportQueryErrorsDoNotConsumeVisibilitySamples(t *testing.T) {
	fx := newFixture(t)
	report := newRunReport(
		fx.clock.now, TriggerUTC, ReportExecution{}, "mainnet",
		fx.orchestrator.builders, fx.orchestrator.builderNames,
		"0xsettlement", "0xrecipient",
	)
	report.token = cloneToken(&fx.chain.token)
	report.target(PayoutKindReferral).Payout = PayoutReport{
		Status: ReportStepSuccess, Amount: "1.5", SettlementTotalBefore: "2.5",
	}
	fx.chain.balanceErrorSequence["0xsettlement"] = []error{
		errors.New("temporary info failure"), errors.New("temporary info failure"),
	}
	expected := info.SpotBalanceAmounts{
		Total: decimal.NewFromInt(1), Available: decimal.NewFromInt(1),
	}
	fx.chain.balanceSequence["0xsettlement"] = []info.SpotBalanceAmounts{expected, expected}

	fx.orchestrator.observeSettlementFinalBalance(context.Background(), &report)

	if !report.SettlementBalance.Observed || fx.chain.balanceCalls["0xsettlement"] != 4 {
		t.Fatalf("settlement balance = %#v, calls=%d",
			report.SettlementBalance, fx.chain.balanceCalls["0xsettlement"])
	}
}

func TestFinalReportAllowsConfirmationAfterOrdinaryVisibilityWindow(t *testing.T) {
	fx := newFixture(t)
	report := newRunReport(
		fx.clock.now, TriggerUTC, ReportExecution{}, "mainnet",
		fx.orchestrator.builders, fx.orchestrator.builderNames,
		"0xsettlement", "0xrecipient",
	)
	report.token = cloneToken(&fx.chain.token)
	report.target(PayoutKindReferral).Payout = PayoutReport{
		Status: ReportStepSuccess, Amount: "1.5", SettlementTotalBefore: "2.5",
	}
	stale := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("2.5"), Available: decimal.RequireFromString("2.5"),
	}
	expected := info.SpotBalanceAmounts{
		Total: decimal.NewFromInt(1), Available: decimal.NewFromInt(1),
	}
	fx.chain.balanceSequence["0xsettlement"] = []info.SpotBalanceAmounts{
		stale, stale, stale, stale, expected, expected,
	}

	fx.orchestrator.observeSettlementFinalBalance(context.Background(), &report)

	if !report.SettlementBalance.Observed || fx.chain.balanceCalls["0xsettlement"] != 6 {
		t.Fatalf("settlement balance = %#v, calls=%d",
			report.SettlementBalance, fx.chain.balanceCalls["0xsettlement"])
	}
}

func TestFinalReportFailsClosedWhenSuccessfulPayoutBoundaryIsInvalid(t *testing.T) {
	fx := newFixture(t)
	report := newRunReport(
		fx.clock.now, TriggerUTC, ReportExecution{}, "mainnet",
		fx.orchestrator.builders, fx.orchestrator.builderNames,
		"0xsettlement", "0xrecipient",
	)
	report.token = cloneToken(&fx.chain.token)
	report.target(PayoutKindReferral).Payout = PayoutReport{Status: ReportStepSuccess, Amount: "1.5"}

	fx.orchestrator.observeSettlementFinalBalance(context.Background(), &report)

	if !report.SettlementBalance.PostPayoutUnconfirmed ||
		report.SettlementBalance.Observed || fx.chain.balanceCalls["0xsettlement"] != 0 ||
		!containsString(
			report.Warnings,
			"Settlement: Payout 已成功但付款后余额边界无效，任务结束余额无法确认",
		) {
		t.Fatalf("report = %#v, calls=%d", report, fx.chain.balanceCalls["0xsettlement"])
	}
}

func TestDailyConsolidationRunsOnlyOncePerUTCDate(t *testing.T) {
	fx := newFixture(t)
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantEvents := append([]string(nil), fx.chain.events...)
	wantClaimCalls := fx.chain.claimableCalls["0xbuilder"]
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !equalStrings(fx.chain.events, wantEvents) ||
		fx.chain.claimableCalls["0xbuilder"] != wantClaimCalls {
		t.Fatalf("duplicate daily consolidation executed actions: %v", fx.chain.events)
	}
	assertLogFields(t, fx.logger, "funding_daily_consolidation_skipped", map[string]any{
		"utc_date": "2026-07-12",
	})
}

func TestDailyClaimablesArePersistedBeforeClaimSubmission(t *testing.T) {
	fx := newFixture(t)
	fx.store.consolidationSaved = make(chan DailyConsolidation)
	fx.chain.claimStarted = make(chan string, 1)
	fx.chain.claimRelease = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- fx.orchestrator.RunDailyConsolidation(context.Background())
	}()

	captured := receiveWithin(t, fx.store.consolidationSaved)
	if captured.Status != DailyConsolidationClaimablesCaptured ||
		len(captured.Builders) != 1 || !captured.Builders[0].ClaimableObserved ||
		captured.Builders[0].ClaimableUSDC != "1.5" ||
		captured.Builders[0].ClaimStatus != ReportStepPending ||
		!captured.CompletedAt.IsZero() {
		t.Fatalf("captured daily record = %#v", captured)
	}
	if builder := receiveWithin(t, fx.chain.claimStarted); builder != "0xbuilder" {
		t.Fatalf("claim signer = %q", builder)
	}
	close(fx.chain.claimRelease)
	completed := receiveWithin(t, fx.store.consolidationSaved)
	if completed.Status != DailyConsolidationCompleted || completed.CompletedAt.IsZero() ||
		len(completed.Builders) != 1 || completed.Builders[0].ClaimableUSDC != "1.5" {
		t.Fatalf("completed daily record = %#v", completed)
	}
	if err := receiveWithin(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestDailyClaimDoesNotStartWhenClaimableRecordCannotBeSaved(t *testing.T) {
	fx := newFixture(t)
	fx.store.consolidationSaveErr = errors.New("disk unavailable")
	err := fx.orchestrator.RunDailyConsolidation(context.Background())
	if err == nil || !strings.Contains(err.Error(), "save captured daily claimables") {
		t.Fatalf("RunDailyConsolidation() error = %v", err)
	}
	if containsString(fx.chain.events, "submit:claimRewards:0xbuilder") ||
		containsString(fx.chain.events, "submit:spotSend:0xbuilder") {
		t.Fatalf("actions ran without a saved claimable boundary: %v", fx.chain.events)
	}
}

func TestDailyClaimableQueriesAndClaimsRunInParallel(t *testing.T) {
	fx := newFixture(t)
	orchestrator := newTwoBuilderOrchestrator(t, fx)
	fx.chain.claimableStarted = make(chan string, 2)
	fx.chain.claimableRelease = make(chan struct{})
	fx.chain.claimStarted = make(chan string, 2)
	fx.chain.claimRelease = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- orchestrator.RunDailyConsolidation(context.Background())
	}()

	queryBuilders := []string{
		receiveWithin(t, fx.chain.claimableStarted),
		receiveWithin(t, fx.chain.claimableStarted),
	}
	if !sameStrings(queryBuilders, []string{"0xbuilder", "0xbuilder-2"}) {
		t.Fatalf("parallel claimable builders = %v", queryBuilders)
	}
	fx.chain.mu.Lock()
	fx.chain.claimableStarted = nil
	fx.chain.mu.Unlock()
	close(fx.chain.claimableRelease)
	claimBuilders := []string{
		receiveWithin(t, fx.chain.claimStarted),
		receiveWithin(t, fx.chain.claimStarted),
	}
	if !sameStrings(claimBuilders, []string{"0xbuilder", "0xbuilder-2"}) {
		t.Fatalf("parallel claim builders = %v", claimBuilders)
	}
	close(fx.chain.claimRelease)
	if err := receiveWithin(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestDailyClaimableQueryRetriesOnlyFailedBuilders(t *testing.T) {
	fx := newFixture(t)
	orchestrator := newTwoBuilderOrchestrator(t, fx)
	fx.chain.claimableErrorSequence["0xbuilder"] = []error{
		errors.New("temporary info failure"), nil,
	}
	if err := orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fx.chain.claimableCalls["0xbuilder"] != 2 ||
		fx.chain.claimableCalls["0xbuilder-2"] != 1 {
		t.Fatalf("claimable calls = %#v", fx.chain.claimableCalls)
	}
	if countDelay(fx.sleeper.delays, claimableQueryRetryInterval) != 1 {
		t.Fatalf("retry delays = %v", fx.sleeper.delays)
	}
	record := fx.store.consolidations["2026-07-12"]
	if record.Builders[0].ClaimableQueryAttempts != 2 ||
		record.Builders[1].ClaimableQueryAttempts != 1 {
		t.Fatalf("daily query attempts = %#v", record.Builders)
	}
}

func TestExhaustedClaimableQueryConsumesExactBelowThresholdRejection(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimableErrors["0xbuilder"] = errors.New("info unavailable")
	fx.chain.claimResult = exchange.SubmitResult{
		Rejected: true,
		Response: json.RawMessage(
			`{"status":"err","response":"Must have at least 1 of a reward token to claim"}`,
		),
	}
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	record := fx.store.consolidations["2026-07-12"]
	if len(record.Builders) != 1 || record.Builders[0].ClaimableObserved ||
		record.Builders[0].ClaimableQueryAttempts != claimableQueryAttempts ||
		record.Builders[0].ClaimAttempts != 1 ||
		record.Builders[0].ClaimStatus != ReportStepSkipped ||
		record.Builders[0].ClaimSummary != "未达到 1 USDC 领取门槛（由交易所确认）" ||
		countString(fx.chain.events, "submit:claimRewards:0xbuilder") != 1 {
		t.Fatalf("incomplete daily record = %#v, events = %v", record, fx.chain.events)
	}
	assertLogFields(t, fx.logger, "funding_builder_claimable_query_failed", map[string]any{
		"builder": "0xbuilder", "error": "info unavailable",
	})
	assertLogFields(t, fx.logger, "funding_builder_claim_eligibility_checked", map[string]any{
		"builder": "0xbuilder", "claimable_observed": false, "eligible": true,
		"claimable_usdc": nil,
	})
	assertLogFields(t, fx.logger, "funding_builder_claim_below_threshold_confirmed", map[string]any{
		"builder": "0xbuilder", "exchange_message": claimBelowThresholdRejection,
	})
	if containsLogFields(fx.logger, "funding_builder_action_failed", map[string]any{
		"builder": "0xbuilder", "action_kind": "claimRewards",
	}) {
		t.Fatalf("exact below-threshold rejection was reported as a claim failure: %#v", record)
	}

	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("1.5")
	fx.orchestrator.clock = fixedClock{now: time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)}
	if err := fx.orchestrator.RunScheduled(context.Background(), TriggerUTC, 1, 6); err != nil {
		t.Fatal(err)
	}
	report := onlyReport(t, fx.notifier)
	if report.DailyConsolidation.ObservedBuilders != 0 ||
		report.DailyConsolidation.TotalClaimableUSDC != "" ||
		report.DailyConsolidation.CoversPayout || report.DailyConsolidation.BelowPayout ||
		!containsString(report.Warnings, "UTC 00:00 Claimable 仅成功查询 0/1 个 Builder，当日对账数据不完整") {
		t.Fatalf("incomplete reconciliation report = %#v", report)
	}
}

func TestExhaustedClaimableQueryStillClaimsAndSweepsAcceptedReward(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimableErrors["0xbuilder"] = errors.New("info unavailable")

	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}

	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.ClaimableObserved || builder.ClaimAttempts != 1 ||
		builder.ClaimStatus != ReportStepSuccess ||
		builder.ClaimSummary != "Reward 已领取；零点 Claimable 数据缺失" ||
		builder.SweepStatus != ReportStepSuccess || builder.SweepAmount != "1.5" ||
		countString(fx.chain.events, "submit:claimRewards:0xbuilder") != 1 ||
		countString(fx.chain.events, "submit:spotSend:0xbuilder") != 1 {
		t.Fatalf("fallback claim builder = %#v, events = %v", builder, fx.chain.events)
	}
}

func TestExhaustedClaimableQueryAcceptsExactlyOneRewardFromExchange(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimableRewards["0xbuilder"] = decimal.NewFromInt(1)
	fx.chain.claimableErrors["0xbuilder"] = errors.New("info unavailable")

	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}

	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.ClaimStatus != ReportStepSuccess || builder.ClaimUncertain ||
		builder.SweepStatus != ReportStepSuccess || builder.SweepAmount != "1" ||
		countString(fx.chain.events, "submit:claimRewards:0xbuilder") != 1 {
		t.Fatalf("one-USDC fallback claim = %#v, events = %v", builder, fx.chain.events)
	}
}

func TestClaimBelowThresholdRequiresExactStructuredMessage(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    bool
	}{
		{name: "exact", message: claimBelowThresholdRejection, want: true},
		{name: "trailing space", message: claimBelowThresholdRejection + " "},
		{name: "prefix", message: "exchange: " + claimBelowThresholdRejection},
		{name: "case", message: strings.ToLower(claimBelowThresholdRejection)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"response": tt.message, "status": "err"})
			if err != nil {
				t.Fatal(err)
			}
			got := claimBelowThreshold(exchange.SubmitResult{Rejected: true, Response: body})
			if got != tt.want {
				t.Fatalf("claimBelowThreshold() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuilderSubmitResponseLogIsBoundedAndRedacted(t *testing.T) {
	fx := newFixture(t)
	response := `{"status":"err","accessToken":"raw-secret","response":"` +
		strings.Repeat("x", 2048) + `"}`
	fx.orchestrator.logBuilderSubmitResult(
		context.Background(), &RunState{RunID: "run"}, "0xbuilder", "claimRewards", "",
		exchange.SubmitResult{Rejected: true, Response: json.RawMessage(response)}, nil,
	)
	for _, entry := range fx.logger.entries {
		if entry.attrs["event"] != "funding_builder_submit_result" {
			continue
		}
		logged, _ := entry.attrs["response"].(string)
		if strings.Contains(logged, "raw-secret") || len(logged) > 1024 ||
			!strings.Contains(logged, "[MASKED]") || !strings.Contains(logged, "...[truncated]") {
			t.Fatalf("logged response = %q (%d bytes)", logged, len(logged))
		}
		return
	}
	t.Fatal("funding_builder_submit_result was not logged")
}

func TestObservedClaimableDoesNotConsumeBelowThresholdRejection(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimResult = exchange.SubmitResult{
		Rejected: true,
		Response: json.RawMessage(
			`{"status":"err","response":"Must have at least 1 of a reward token to claim"}`,
		),
	}
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if !builder.ClaimableObserved || builder.ClaimStatus != ReportStepWarning ||
		builder.ClaimAttempts != 1 || containsLogEvent(fx.logger, "funding_builder_claim_below_threshold_confirmed") {
		t.Fatalf("observed rejected claim = %#v", builder)
	}
	assertLogFields(t, fx.logger, "funding_builder_action_failed", map[string]any{
		"builder": "0xbuilder", "action_kind": "claimRewards", "outcome": "rejected",
	})
}

func TestClaimSubmitErrorCannotConsumeBelowThresholdResponse(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimableErrors["0xbuilder"] = errors.New("info unavailable")
	fx.chain.claimResult = exchange.SubmitResult{
		Rejected: true,
		Response: json.RawMessage(
			`{"status":"err","response":"Must have at least 1 of a reward token to claim"}`,
		),
	}
	fx.chain.claimSubmitErr = errors.New("exchange transport interrupted")
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.ClaimStatus != ReportStepWarning || !builder.ClaimUncertain ||
		builder.ClaimAttempts != 1 || containsLogEvent(fx.logger, "funding_builder_claim_below_threshold_confirmed") {
		t.Fatalf("transport-error claim = %#v", builder)
	}
	assertLogFields(t, fx.logger, "funding_builder_submit_result", map[string]any{
		"builder": "0xbuilder", "error": "exchange transport interrupted",
	})
}

func TestExhaustedClaimableQueryDoesNotConsumeNonExactRejection(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimableErrors["0xbuilder"] = errors.New("info unavailable")
	fx.chain.claimResult = exchange.SubmitResult{
		Rejected: true,
		Response: json.RawMessage(
			`{"status":"err","response":"Must have at least 1 of a reward token to claim "}`,
		),
	}
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.ClaimStatus != ReportStepWarning || builder.ClaimAttempts != 1 ||
		containsLogEvent(fx.logger, "funding_builder_claim_below_threshold_confirmed") ||
		!containsLogFields(fx.logger, "funding_builder_action_failed", map[string]any{
			"builder": "0xbuilder", "action_kind": "claimRewards", "outcome": "rejected",
		}) {
		t.Fatalf("non-exact fallback rejection = %#v", builder)
	}
}

func TestExhaustedClaimableQueryAmbiguousClaimIsNeverResubmitted(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(fmt.Sprintf("applied=%t", applied), func(t *testing.T) {
			fx := newFixture(t)
			fx.chain.claimableErrors["0xbuilder"] = errors.New("info unavailable")
			fx.chain.claimResult = exchange.SubmitResult{}
			fx.chain.applyUnknownClaim = applied

			if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
				t.Fatal(err)
			}

			builder := fx.store.consolidations["2026-07-12"].Builders[0]
			if builder.ClaimStatus != ReportStepWarning || !builder.ClaimUncertain ||
				builder.ClaimAttempts != 1 ||
				countString(fx.chain.events, "submit:claimRewards:0xbuilder") != 1 {
				t.Fatalf("ambiguous fallback claim = %#v, events = %v", builder, fx.chain.events)
			}
			if applied {
				if builder.SweepAmount != "1.5" || builder.SweepStatus != ReportStepWarning {
					t.Fatalf("applied ambiguous fallback was not swept best-effort: %#v", builder)
				}
			} else if builder.SweepAmount != "" || builder.SweepStatus != ReportStepWarning {
				t.Fatalf("unapplied ambiguous fallback = %#v", builder)
			}
		})
	}
}

func TestClaimableQueryRetryReturningZeroSkipsClaim(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimableRewards["0xbuilder"] = decimal.Zero
	fx.chain.claimableErrorSequence["0xbuilder"] = []error{
		errors.New("temporary info failure"), nil,
	}

	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}

	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if !builder.ClaimableObserved || builder.ClaimableUSDC != "0" ||
		builder.ClaimableQueryAttempts != 2 || builder.ClaimAttempts != 0 ||
		builder.ClaimStatus != ReportStepSkipped ||
		containsString(fx.chain.events, "submit:claimRewards:0xbuilder") {
		t.Fatalf("zero claimable builder = %#v, events = %v", builder, fx.chain.events)
	}
}

func TestUncertainClaimUsesRelativeBalanceEvidenceWithoutResubmission(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimResult = exchange.SubmitResult{}
	fx.chain.applyUnknownClaim = true
	preClaim := info.SpotBalanceAmounts{Total: decimal.Zero, Available: decimal.Zero}
	postClaim := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("1.5"), Available: decimal.RequireFromString("1.5"),
	}
	fx.chain.balanceSequence["0xbuilder"] = []info.SpotBalanceAmounts{
		preClaim, // Stable pre-claim baseline.
		preClaim,
		preClaim, // Stale post-action observation.
		postClaim,
	}
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.ClaimAttempts != 1 || builder.ClaimUncertain ||
		builder.ClaimStatus != ReportStepSuccess ||
		builder.ClaimSummary != "Reward 已领取（由余额增长确认；未重发）" ||
		builder.SweepAmount != "1.5" ||
		countString(fx.chain.events, "submit:claimRewards:0xbuilder") != 1 {
		t.Fatalf("observed claim builder = %#v, events=%v delays=%v", builder, fx.chain.events, fx.sleeper.delays)
	}
}

func TestUncertainSweepRetriesLatestAvailableUntilAccepted(t *testing.T) {
	fx := newFixture(t)
	fx.chain.sweepResults = map[string][]exchange.SubmitResult{
		"0xbuilder": {{}, {Accepted: true}},
	}

	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.SweepStatus != ReportStepWarning || builder.SweepAmount != "1.5" ||
		builder.SweepSummary != "第 2 次归集已接受；此前响应未知，累计归集金额无法确认" ||
		countString(fx.chain.events, "submit:spotSend:0xbuilder") != 2 ||
		!fx.chain.balances["0xbuilder"].IsZero() ||
		!fx.chain.balances["0xsettlement"].Equal(decimal.RequireFromString("1.5")) {
		t.Fatalf("retried sweep = %#v, balances=%v events=%v",
			builder, fx.chain.balances, fx.chain.events)
	}
	assertDistinctSweepNonces(t, fx.chain.submittedActions, "0xbuilder", 2)
}

func TestUncertainSweepRetriesLastAmountWhenStableQueryReturnsHistoricalZero(t *testing.T) {
	fx := newFixture(t)
	fx.chain.sweepResults = map[string][]exchange.SubmitResult{
		"0xbuilder": {{}, {Accepted: true}},
	}
	zero := info.SpotBalanceAmounts{Total: decimal.Zero, Available: decimal.Zero}
	postClaim := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("1.5"), Available: decimal.RequireFromString("1.5"),
	}
	fx.chain.balanceSequence["0xbuilder"] = []info.SpotBalanceAmounts{
		zero, zero, // Claim baseline.
		postClaim, postClaim, // Claim visibility.
		postClaim, postClaim, // Initial sweep amount.
		zero, zero, // Historical zero returned before retry.
	}

	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.SweepStatus != ReportStepWarning || builder.SweepAmount != "1.5" ||
		countString(fx.chain.events, "submit:spotSend:0xbuilder") != 2 ||
		!fx.chain.balances["0xbuilder"].IsZero() ||
		!fx.chain.balances["0xsettlement"].Equal(decimal.RequireFromString("1.5")) {
		t.Fatalf("stale-zero sweep = %#v, balances=%v events=%v",
			builder, fx.chain.balances, fx.chain.events)
	}
}

func TestUncertainSweepDoesNotTrustLowerHistoricalPositiveBalance(t *testing.T) {
	fx := newFixture(t)
	fx.chain.sweepResults = map[string][]exchange.SubmitResult{
		"0xbuilder": {{}, {Accepted: true}},
	}
	zero := info.SpotBalanceAmounts{Total: decimal.Zero, Available: decimal.Zero}
	postClaim := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("1.5"), Available: decimal.RequireFromString("1.5"),
	}
	historicalPartial := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("0.5"), Available: decimal.RequireFromString("0.5"),
	}
	fx.chain.balanceSequence["0xbuilder"] = []info.SpotBalanceAmounts{
		zero, zero,
		postClaim, postClaim,
		postClaim, postClaim,
		historicalPartial, historicalPartial,
	}

	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	actions := sweepActionsFor(fx.chain.submittedActions, "0xbuilder")
	if builder.SweepStatus != ReportStepWarning || builder.SweepAmount != "1.5" ||
		len(actions) != 2 || actions[1].Amount != "1.5" ||
		!fx.chain.balances["0xbuilder"].IsZero() ||
		!fx.chain.balances["0xsettlement"].Equal(decimal.RequireFromString("1.5")) {
		t.Fatalf("lower historical balance retry = %#v, actions=%#v balances=%v",
			builder, actions, fx.chain.balances)
	}
}

func TestAppliedUncertainSweepRetriesSafelyAndRemainsWarning(t *testing.T) {
	fx := newFixture(t)
	fx.chain.sweepResult = exchange.SubmitResult{}
	fx.chain.applyUnknownSweep = true
	zero := info.SpotBalanceAmounts{Total: decimal.Zero, Available: decimal.Zero}
	postClaim := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("1.5"), Available: decimal.RequireFromString("1.5"),
	}
	fx.chain.balanceSequence["0xbuilder"] = []info.SpotBalanceAmounts{
		zero, zero,
		postClaim, postClaim,
		postClaim, postClaim,
		postClaim, postClaim, // Stale positive snapshots before the first retry.
	}

	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.SweepStatus != ReportStepWarning || builder.SweepAmount != "" ||
		countString(fx.chain.events, "submit:spotSend:0xbuilder") != sweepSubmitAttempts ||
		!fx.chain.balances["0xbuilder"].IsZero() ||
		!fx.chain.balances["0xsettlement"].Equal(decimal.RequireFromString("1.5")) {
		t.Fatalf("applied uncertain sweep = %#v, balances=%v events=%v",
			builder, fx.chain.balances, fx.chain.events)
	}
	assertDistinctSweepNonces(t, fx.chain.submittedActions, "0xbuilder", sweepSubmitAttempts)
}

func TestUncertainThenRejectedSweepStillRetriesUntilAccepted(t *testing.T) {
	fx := newFixture(t)
	fx.chain.sweepResults = map[string][]exchange.SubmitResult{
		"0xbuilder": {{}, {Rejected: true}, {Accepted: true}},
	}

	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.SweepStatus != ReportStepWarning || builder.SweepAmount != "1.5" ||
		builder.SweepSummary != "第 3 次归集已接受；此前响应未知，累计归集金额无法确认" ||
		countString(fx.chain.events, "submit:spotSend:0xbuilder") != 3 ||
		!fx.chain.balances["0xsettlement"].Equal(decimal.RequireFromString("1.5")) {
		t.Fatalf("unknown/rejected/accepted sweep = %#v, balances=%v events=%v",
			builder, fx.chain.balances, fx.chain.events)
	}
}

func TestUncertainSweepExhaustionDoesNotBlockLaterBuilders(t *testing.T) {
	fx := newFixture(t)
	orchestrator := newTwoBuilderOrchestrator(t, fx)
	fx.chain.sweepResults = map[string][]exchange.SubmitResult{
		"0xbuilder": {{}, {}, {}},
	}

	if err := orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	record := fx.store.consolidations["2026-07-12"]
	if len(record.Builders) != 2 ||
		record.Builders[0].SweepStatus != ReportStepWarning ||
		record.Builders[0].SweepAmount != "" ||
		record.Builders[1].SweepStatus != ReportStepSuccess ||
		countString(fx.chain.events, "submit:spotSend:0xbuilder") != sweepSubmitAttempts ||
		countString(fx.chain.events, "submit:spotSend:0xbuilder-2") != 1 ||
		!fx.chain.balances["0xsettlement"].Equal(decimal.RequireFromString("1.25")) {
		t.Fatalf("builders=%#v balances=%v events=%v",
			record.Builders, fx.chain.balances, fx.chain.events)
	}
}

func TestAcceptedSweepContinuesToNextBuilder(t *testing.T) {
	fx := newFixture(t)
	orchestrator := newTwoBuilderOrchestrator(t, fx)

	if err := orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	record := fx.store.consolidations["2026-07-12"]
	if len(record.Builders) != 2 ||
		record.Builders[0].SweepStatus != ReportStepSuccess ||
		record.Builders[1].SweepStatus != ReportStepSuccess ||
		countString(fx.chain.events, "submit:spotSend:0xbuilder") != 1 ||
		countString(fx.chain.events, "submit:spotSend:0xbuilder-2") != 1 ||
		!fx.chain.balances["0xsettlement"].Equal(decimal.RequireFromString("2.75")) {
		t.Fatalf("builders=%#v balances=%v events=%v",
			record.Builders, fx.chain.balances, fx.chain.events)
	}
}

func TestAcceptedSweepDoesNotExposePreSubmitBalanceAsFinal(t *testing.T) {
	fx := newFixture(t)
	report, err := fx.orchestrator.runConsolidation(
		context.Background(), TriggerConsolidateOnly, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Builders) != 1 || report.Builders[0].SweepStatus != ReportStepSuccess ||
		report.Builders[0].FinalTotal != "" || report.Builders[0].FinalAvailable != "" ||
		report.stage("finalize").Status != ReportStepSkipped {
		t.Fatalf("builder report = %#v", report.Builders)
	}
}

func TestUncertainClaimRejectsStaleLowBaselineAndUnchangedClaimable(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimResult = exchange.SubmitResult{}
	fx.chain.balances["0xbuilder"] = decimal.NewFromInt(2)
	fx.chain.balanceSequence["0xbuilder"] = []info.SpotBalanceAmounts{
		{Total: decimal.Zero, Available: decimal.Zero}, // Historical pre-claim baseline.
	}

	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if !builder.ClaimUncertain || builder.ClaimStatus != ReportStepWarning ||
		countString(fx.chain.events, "submit:claimRewards:0xbuilder") != 1 {
		t.Fatalf("stale-baseline claim = %#v, events=%v", builder, fx.chain.events)
	}
}

func TestRejectedSweepDoesNotRetry(t *testing.T) {
	fx := newFixture(t)
	fx.chain.sweepResult = exchange.SubmitResult{Rejected: true}
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.SweepStatus != ReportStepWarning ||
		countString(fx.chain.events, "submit:spotSend:0xbuilder") != 1 {
		t.Fatalf("rejected sweep = %#v, events=%v delays=%v", builder, fx.chain.events, fx.sleeper.delays)
	}
}

func TestScheduledFundingNeverClaimsOrSweepsWithoutDailyRecord(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("1.5")
	fx.orchestrator.clock = fixedClock{now: time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)}
	if err := fx.orchestrator.RunScheduled(context.Background(), TriggerUTC, 1, 6); err != nil {
		t.Fatal(err)
	}
	if !equalStrings(fx.chain.events, []string{"submit:spotSend:0xsettlement"}) {
		t.Fatalf("UTC 01:00 chain events = %v, want payout only", fx.chain.events)
	}
	if fx.chain.claimableCalls["0xbuilder"] != 0 || fx.chain.balanceCalls["0xbuilder"] != 0 {
		t.Fatal("UTC 01:00 accessed builder claim or balance APIs")
	}
	report := onlyReport(t, fx.notifier)
	if report.Status != ReportStatusWarning || !report.DailyConsolidation.Missing ||
		!containsString(report.Warnings, "未找到 UTC 00:00 归集记录") {
		t.Fatalf("missing-consolidation report = %#v", report)
	}
}

func TestUnderfundedFundingRetryDoesNotFallBackToBuilderActions(t *testing.T) {
	fx := newFixture(t)
	fx.orchestrator.clock = fixedClock{now: time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)}
	err := fx.orchestrator.RunScheduled(context.Background(), TriggerUTC, 1, 6)
	if err == nil || IsFatal(err) {
		t.Fatalf("RunScheduled() error = %v, want ordinary underfunded error", err)
	}
	if len(fx.chain.events) != 0 || fx.chain.claimableCalls["0xbuilder"] != 0 ||
		fx.chain.balanceCalls["0xbuilder"] != 0 {
		t.Fatalf("underfunded payout touched builders: events=%v", fx.chain.events)
	}
	if fx.store.current == nil || testActiveTarget(t, fx.store.current).Phase != PhasePrepared {
		t.Fatalf("current = %#v, want prepared retry state", fx.store.current)
	}

	fx.notifier.reports = nil
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("1.5")
	if err := fx.orchestrator.RunScheduled(context.Background(), TriggerUTC, 2, 6); err != nil {
		t.Fatal(err)
	}
	if !equalStrings(fx.chain.events, []string{"submit:spotSend:0xsettlement"}) {
		t.Fatalf("retry events = %v, want payout only", fx.chain.events)
	}
}

func TestPayoutPreflightRetriesStaleUnderfundedBalance(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("1.5")
	zero := info.SpotBalanceAmounts{Total: decimal.Zero, Available: decimal.Zero}
	funded := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("1.5"), Available: decimal.RequireFromString("1.5"),
	}
	fx.chain.balanceSequence["0xsettlement"] = []info.SpotBalanceAmounts{zero, zero, funded}

	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	if countString(fx.chain.events, "submit:spotSend:0xsettlement") != 1 ||
		!equalUint64(fx.repo.completedIDs, []uint64{1, 2}) {
		t.Fatalf("events=%v completed=%v", fx.chain.events, fx.repo.completedIDs)
	}
	if report := onlyReport(t, fx.notifier); report.target(PayoutKindReferral).Payout.SettlementTotalBefore != "1.5" {
		t.Fatalf("payout total before = %q, want final funded sample",
			report.target(PayoutKindReferral).Payout.SettlementTotalBefore)
	}
}

func TestPayoutPreflightRetriesQueryErrorBeforeFundedBalance(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("1.5")
	fx.chain.balanceErrorSequence["0xsettlement"] = []error{
		errors.New("temporary info failure"), nil,
	}

	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	if countString(fx.chain.events, "submit:spotSend:0xsettlement") != 1 ||
		!equalUint64(fx.repo.completedIDs, []uint64{1, 2}) {
		t.Fatalf("events=%v completed=%v", fx.chain.events, fx.repo.completedIDs)
	}
	if report := onlyReport(t, fx.notifier); report.target(PayoutKindReferral).Payout.SettlementTotalBefore != "1.5" {
		t.Fatalf("payout total before = %q, want final funded sample",
			report.target(PayoutKindReferral).Payout.SettlementTotalBefore)
	}
}

func TestPayoutDoesNotQueryRecipientBalance(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("1.5")
	fx.chain.balanceErrors["0xrecipient"] = errors.New("recipient info unavailable")

	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	if countString(fx.chain.events, "submit:spotSend:0xsettlement") != 1 ||
		fx.chain.balanceCalls["0xrecipient"] != 0 ||
		!equalUint64(fx.repo.completedIDs, []uint64{1, 2}) {
		t.Fatalf("events=%v recipient calls=%d completed=%v",
			fx.chain.events, fx.chain.balanceCalls["0xrecipient"], fx.repo.completedIDs)
	}
}

func TestClaimableBelowPayoutWarnsWithoutBlockingFundedPayout(t *testing.T) {
	fx := newFixture(t)
	fx.repo.records = append(fx.repo.records,
		Record{ID: 9, PeriodStartAt: 30, Amount: "0.250000000000000001"},
	)
	fx.chain.claimableRewards["0xbuilder"] = decimal.RequireFromString("1.6")
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("1.750001")
	fx.orchestrator.clock = fixedClock{now: time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)}
	if err := fx.orchestrator.RunScheduled(context.Background(), TriggerUTC, 1, 6); err != nil {
		t.Fatal(err)
	}
	report := onlyReport(t, fx.notifier)
	warning := "UTC 00:00 Claimable 合计 1.6 USDC，低于 Referral Payout 1.750001 USDC，短缺 0.150001 USDC，请人工核查结算口径"
	if report.Status != ReportStatusWarning ||
		report.PayoutTotal != "1.750001" ||
		report.target(PayoutKindReferral).Payout.Status != ReportStepSuccess ||
		!report.DailyConsolidation.BelowPayout ||
		report.DailyConsolidation.ShortfallUSDC != "0.150001" ||
		!containsString(report.Warnings, warning) {
		t.Fatalf("claimed reconciliation report = %#v", report)
	}
}

func TestUncertainClaimIsObservedWithoutResubmission(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimResult = exchange.SubmitResult{}
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	record := fx.store.consolidations["2026-07-12"]
	if len(record.Builders) != 1 || !record.Builders[0].ClaimUncertain ||
		record.Builders[0].ClaimableUSDC != "1.5" ||
		record.Builders[0].ClaimAttempts != 1 ||
		countString(fx.chain.events, "submit:claimRewards:0xbuilder") != 1 {
		t.Fatalf("uncertain daily consolidation = %#v", record)
	}
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("1.5")
	fx.orchestrator.clock = fixedClock{now: time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)}
	if err := fx.orchestrator.RunScheduled(context.Background(), TriggerUTC, 1, 6); err != nil {
		t.Fatal(err)
	}
	report := onlyReport(t, fx.notifier)
	if report.DailyConsolidation.UncertainClaims != 1 ||
		!report.DailyConsolidation.CoversPayout || report.DailyConsolidation.BelowPayout ||
		!containsString(report.Warnings, "UTC 00:00 有 1 个 Builder 的 claim 在有限观察后结果仍不确定，请人工核查") {
		t.Fatalf("uncertain claim report = %#v", report)
	}
}

func TestClaimBelowThresholdIsRecordedWithoutSubmission(t *testing.T) {
	for _, amount := range []string{"0", "0.999999"} {
		t.Run(amount, func(t *testing.T) {
			fx := newFixture(t)
			fx.chain.claimableRewards["0xbuilder"] = decimal.RequireFromString(amount)
			fx.chain.balances["0xbuilder"] = decimal.Zero
			if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
				t.Fatal(err)
			}
			if containsString(fx.chain.events, "submit:claimRewards:0xbuilder") {
				t.Fatalf("claim was submitted with %s claimable USDC", amount)
			}
			builder := fx.store.consolidations["2026-07-12"].Builders[0]
			if builder.ClaimableUSDC != amount || !builder.ClaimableObserved ||
				builder.ClaimStatus != ReportStepSkipped || builder.ClaimAttempts != 0 {
				t.Fatalf("daily builder = %#v", builder)
			}
		})
	}
}

func TestClaimAtThresholdIsSubmitted(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimableRewards["0xbuilder"] = decimal.NewFromInt(1)

	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}

	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if !builder.ClaimableObserved || builder.ClaimableUSDC != "1" ||
		builder.ClaimStatus != ReportStepSuccess || builder.ClaimAttempts != 1 ||
		builder.SweepAmount != "1" ||
		countString(fx.chain.events, "submit:claimRewards:0xbuilder") != 1 {
		t.Fatalf("threshold claim builder = %#v, events = %v", builder, fx.chain.events)
	}
}

func TestConsolidateOnlyDoesNotReadOrWriteDailyRecord(t *testing.T) {
	fx := newFixture(t)
	fx.store.current = &RunState{RunID: "existing"}
	fx.store.consolidations = map[string]DailyConsolidation{
		"2026-07-12": {
			UTCDate: "2026-07-12", Status: DailyConsolidationCompleted,
			StartedAt: fx.clock.now, ClaimablesCapturedAt: fx.clock.now, CompletedAt: fx.clock.now,
			Builders: []DailyConsolidationBuilder{{
				Address: "0xbuilder", ClaimableUSDC: "9", ClaimableObserved: true,
				ClaimableQueryAttempts: 1,
				ClaimStatus:            ReportStepSuccess, SweepStatus: ReportStepSuccess,
			}},
		},
	}
	consolidator, err := NewConsolidator(ConsolidatorConfig{
		Chain: fx.chain, Notifier: fx.notifier, Logger: fx.logger,
		Builders: []string{"0xbuilder"}, BuilderNames: map[string]string{"0xbuilder": "builder-1"},
		Settlement: "0xsettlement", Network: "mainnet",
		Clock: fx.clock, Nonce: &fakeNonce{next: uint64(fx.clock.now.UnixMilli())},
		Sleeper: fx.sleeper,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := consolidator.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !equalStrings(fx.chain.events, []string{
		"submit:claimRewards:0xbuilder", "submit:spotSend:0xbuilder",
	}) {
		t.Fatalf("consolidate-only events = %v", fx.chain.events)
	}
	if fx.repo.listCalls != 0 || len(fx.repo.completedIDs) != 0 ||
		len(fx.store.consolidations) != 1 ||
		fx.store.consolidations["2026-07-12"].Builders[0].ClaimableUSDC != "9" ||
		len(fx.store.saved) != 0 ||
		fx.store.current == nil || fx.store.current.RunID != "existing" {
		t.Fatalf("consolidate-only touched durable state: %#v", fx.store)
	}
	if len(fx.notifier.reports) != 0 {
		t.Fatalf("consolidate-only delivered final reports: %#v", fx.notifier.reports)
	}
}

func TestDailyConsolidationUsesOneSharedClaimVisibilityWait(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimableRewards["0xbuilder-2"] = decimal.RequireFromString("1.25")
	fx.chain.balances["0xbuilder-2"] = decimal.Zero
	orchestrator, err := NewOrchestrator(OrchestratorConfig{
		ReferralRepository: fx.repo,
		Store:              fx.store, DailyConsolidations: fx.store, Chain: fx.chain,
		Notifier: fx.notifier, Logger: fx.logger,
		Builders:     []string{"0xbuilder", "0xbuilder-2"},
		BuilderNames: map[string]string{"0xbuilder": "builder-1", "0xbuilder-2": "builder-2"},
		Settlement:   "0xsettlement", ReferralRecipient: "0xrecipient",
		Clock: fx.clock, Nonce: &fakeNonce{next: uint64(fx.clock.now.UnixMilli())},
		Sleeper: fx.sleeper,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	// One shared claim-baseline wait, two shared claim-visibility waits and one
	// stable pre-sweep wait per Builder. Accepted internal sweeps do not perform
	// a post-submit balance observation.
	if countDelay(fx.sleeper.delays, claimBalanceObservationInterval) != 5 {
		t.Fatalf("claim visibility waits = %v", fx.sleeper.delays)
	}
	if len(fx.chain.events) != 4 ||
		!sameStrings(fx.chain.events[:2], []string{
			"submit:claimRewards:0xbuilder",
			"submit:claimRewards:0xbuilder-2",
		}) || !equalStrings(fx.chain.events[2:], []string{
		"submit:spotSend:0xbuilder", "submit:spotSend:0xbuilder-2",
	}) {
		t.Fatalf("events = %v", fx.chain.events)
	}
}

func TestClaimVisibilityRequiresIncreaseFromPreClaimTotal(t *testing.T) {
	fx := newFixture(t)
	preClaim := info.SpotBalanceAmounts{
		Total: decimal.NewFromInt(2), Available: decimal.NewFromInt(2),
	}
	postClaim := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("3.5"), Available: decimal.RequireFromString("3.5"),
	}
	fx.chain.balances["0xbuilder"] = preClaim.Total
	fx.chain.balanceSequence["0xbuilder"] = []info.SpotBalanceAmounts{
		preClaim, // Pre-claim baseline.
		preClaim, // Stale values that already exceed the 1.5 claimable amount.
		preClaim,
		postClaim,
	}

	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.ClaimStatus != ReportStepSuccess || builder.ClaimUncertain ||
		builder.SweepStatus != ReportStepSuccess || builder.SweepAmount != "3.5" ||
		countString(fx.chain.events, "submit:spotSend:0xbuilder") != 1 {
		t.Fatalf("relative claim visibility builder=%#v events=%v", builder, fx.chain.events)
	}
}

func TestSweepReloadsStableSourceAfterClaimObservation(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xbuilder"] = decimal.RequireFromString("0.5")
	preClaim := info.SpotBalanceAmounts{Total: decimal.Zero, Available: decimal.Zero}
	claimVisible := info.SpotBalanceAmounts{
		Total:     decimal.RequireFromString("1.5"),
		Available: decimal.RequireFromString("1.5"),
	}
	freshSource := info.SpotBalanceAmounts{
		Total: decimal.NewFromInt(2), Available: decimal.NewFromInt(2),
	}
	fx.chain.balanceSequence["0xbuilder"] = []info.SpotBalanceAmounts{
		preClaim, preClaim,
		claimVisible, claimVisible,
		freshSource, freshSource,
	}

	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.SweepAmount != "2" || builder.SweepStatus != ReportStepSuccess ||
		countString(fx.chain.events, "submit:spotSend:0xbuilder") != 1 {
		t.Fatalf("fresh source sweep=%#v events=%v", builder, fx.chain.events)
	}
}

func TestSweepSourceCannotRegressBelowConfirmedClaimBalance(t *testing.T) {
	fx := newFixture(t)
	preClaim := info.SpotBalanceAmounts{Total: decimal.Zero, Available: decimal.Zero}
	postClaim := info.SpotBalanceAmounts{
		Total:     decimal.RequireFromString("1.5"),
		Available: decimal.RequireFromString("1.5"),
	}
	fx.chain.balanceSequence["0xbuilder"] = []info.SpotBalanceAmounts{
		preClaim, preClaim,
		postClaim, postClaim,
		preClaim, preClaim, // A stable historical regression before sweep.
		postClaim, postClaim,
	}

	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.SweepAmount != "1.5" || builder.SweepStatus != ReportStepSuccess ||
		countString(fx.chain.events, "submit:spotSend:0xbuilder") != 1 {
		t.Fatalf("regressed source sweep=%#v events=%v", builder, fx.chain.events)
	}
}

func TestClaimVisibilityAcceptsCreditAboveCapturedMinimum(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimableRewards["0xbuilder"] = decimal.RequireFromString("1.5000009")
	fx.chain.holds["0xbuilder"] = decimal.RequireFromString("0.0000009")
	zero := info.SpotBalanceAmounts{Total: decimal.Zero, Available: decimal.Zero}
	fx.chain.balanceSequence["0xbuilder"] = []info.SpotBalanceAmounts{zero, zero}

	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.ClaimStatus != ReportStepSuccess || builder.ClaimUncertain ||
		builder.SweepAmount != "1.5" {
		t.Fatalf("high-precision claim builder=%#v", builder)
	}
}

func TestAcceptedClaimCanRemainVisibleWhenRewardsRefillImmediately(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimableAfterClaim = map[string]decimal.Decimal{
		"0xbuilder": decimal.RequireFromString("1.5"),
	}

	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.ClaimStatus != ReportStepSuccess || builder.ClaimUncertain ||
		builder.SweepStatus != ReportStepSuccess || builder.SweepAmount != "1.5" ||
		fx.chain.claimableCalls["0xbuilder"] != 1 {
		t.Fatalf("refilled claim builder=%#v calls=%d",
			builder, fx.chain.claimableCalls["0xbuilder"])
	}
}

func TestClaimVisibilityExhaustionStillRecordsBestEffortSweep(t *testing.T) {
	fx := newFixture(t)
	// After the accepted claim the authoritative balance is the same latest
	// value returned throughout the exhausted visibility window.
	fx.chain.balances["0xbuilder"] = decimal.RequireFromString("0.5")
	fx.chain.holds["0xbuilder"] = decimal.RequireFromString("1.5")
	latest := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("2"), Available: decimal.RequireFromString("0.5"),
	}
	fx.chain.balanceSequence["0xbuilder"] = make(
		[]info.SpotBalanceAmounts,
		claimBaselineRequiredConfirmations+claimBalanceObservationAttempts,
	)
	for index := range fx.chain.balanceSequence["0xbuilder"] {
		fx.chain.balanceSequence["0xbuilder"][index] = latest
	}
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.ClaimStatus != ReportStepWarning ||
		builder.ClaimSummary != "Reward 请求已接受，但余额未确认刷新" ||
		builder.SweepAmount != "0.5" || builder.SweepStatus != ReportStepWarning {
		t.Fatalf("daily builder = %#v", builder)
	}
	if fx.chain.balanceCalls["0xbuilder"] !=
		claimBaselineRequiredConfirmations+claimBalanceObservationAttempts+
			sweepBalanceRequiredConfirmations {
		t.Fatalf("balance calls = %d", fx.chain.balanceCalls["0xbuilder"])
	}
}

func TestClaimVisibilityWarningSurvivesLaterSweepFailure(t *testing.T) {
	fx := newFixture(t)
	zero := info.SpotBalanceAmounts{Total: decimal.Zero, Available: decimal.Zero}
	fx.chain.balanceSequence["0xbuilder"] = make(
		[]info.SpotBalanceAmounts,
		claimBaselineRequiredConfirmations+claimBalanceObservationAttempts,
	)
	for index := range fx.chain.balanceSequence["0xbuilder"] {
		fx.chain.balanceSequence["0xbuilder"][index] = zero
	}
	fx.chain.balanceErrorSequence["0xbuilder"] = make(
		[]error, claimBaselineRequiredConfirmations+claimBalanceObservationAttempts,
	)
	for range sweepBalanceQueryAttempts {
		fx.chain.balanceErrorSequence["0xbuilder"] = append(
			fx.chain.balanceErrorSequence["0xbuilder"], errors.New("builder info unavailable"),
		)
	}

	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	entry := fx.store.consolidations["2026-07-12"].Builders[0]
	if entry.ClaimStatus != ReportStepWarning ||
		entry.ClaimSummary != "Reward 请求已接受，但余额未确认刷新" ||
		entry.SweepStatus != ReportStepWarning ||
		entry.SweepSummary != "余额查询失败" {
		t.Fatalf("persisted builder = %#v", entry)
	}
	report := newRunReport(
		fx.clock.now.Add(time.Hour), TriggerUTC, ReportExecution{}, "mainnet",
		fx.orchestrator.builders, fx.orchestrator.builderNames,
		"0xsettlement", "0xrecipient",
	)
	fx.orchestrator.attachDailyConsolidation(
		context.Background(), &report, "2026-07-12",
	)
	if report.Builders[0].ClaimStatus != ReportStepWarning ||
		!containsString(report.Warnings, "builder-1: Reward 请求已接受，但余额未确认刷新") ||
		!containsString(report.Warnings, "builder-1: 余额查询失败") {
		t.Fatalf("attached report = %#v", report)
	}
}

func TestClaimVisibilityExhaustionWithZeroSourceKeepsWarning(t *testing.T) {
	fx := newFixture(t)
	zero := info.SpotBalanceAmounts{Total: decimal.Zero, Available: decimal.Zero}
	fx.chain.balanceSequence["0xbuilder"] = make(
		[]info.SpotBalanceAmounts,
		claimBaselineRequiredConfirmations+claimBalanceObservationAttempts+
			sweepBalanceRequiredConfirmations,
	)
	for index := range fx.chain.balanceSequence["0xbuilder"] {
		fx.chain.balanceSequence["0xbuilder"][index] = zero
	}

	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.SweepStatus != ReportStepWarning ||
		builder.SweepSummary != "Reward 余额未确认刷新，当前无可归集余额" ||
		builder.SweepAmount != "" {
		t.Fatalf("zero-source unconverged claim = %#v", builder)
	}
}

func TestPayoutUsesAvailableButUncertainConfirmationUsesTotal(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	fx.chain.holds["0xsettlement"] = decimal.NewFromInt(1)
	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || containsString(fx.chain.events, "submit:spotSend:0xsettlement") {
		t.Fatalf("held settlement payout error=%v events=%v", err, fx.chain.events)
	}

	fx = newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	fx.chain.payoutResult = exchange.SubmitResult{}
	fx.chain.balanceSequence["0xsettlement"] = []info.SpotBalanceAmounts{
		{Total: decimal.NewFromInt(2), Available: decimal.NewFromInt(2)},
		{Total: decimal.NewFromInt(2), Available: decimal.NewFromInt(2)},
		{Total: decimal.NewFromInt(2), Available: decimal.NewFromInt(2)},
		{Total: decimal.NewFromInt(2), Available: decimal.RequireFromString("0.4")},
		{Total: decimal.NewFromInt(2), Available: decimal.RequireFromString("0.3")},
		{Total: decimal.NewFromInt(2), Available: decimal.RequireFromString("0.2")},
		{Total: decimal.NewFromInt(2), Available: decimal.RequireFromString("0.1")},
		{Total: decimal.NewFromInt(2), Available: decimal.Zero},
	}
	err = fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || !IsFatal(err) || fx.store.current == nil ||
		testActiveTarget(t, fx.store.current).Phase != PhaseBlocked {
		t.Fatalf("available-only decrease error=%v current=%#v", err, fx.store.current)
	}
	report := onlyReport(t, fx.notifier)
	if !report.SettlementBalance.PostPayoutUnconfirmed ||
		report.SettlementBalance.Observed || report.SettlementBalance.Total != "" ||
		report.SettlementBalance.Available != "" {
		t.Fatalf("ambiguous payout ending balance = %#v", report.SettlementBalance)
	}
}

func TestUnknownPayoutConfirmsOnlyAfterSettlementTotalDecreases(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("1.5")
	fx.chain.payoutResult = exchange.SubmitResult{}
	fx.chain.applyUnknownPayout = true
	before := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("1.5"), Available: decimal.RequireFromString("1.5"),
	}
	after := info.SpotBalanceAmounts{Total: decimal.Zero, Available: decimal.Zero}
	fx.chain.balanceSequence["0xsettlement"] = []info.SpotBalanceAmounts{
		before, before, // Stable payout preflight.
		before, // Stale pre-payout state observed after the ambiguous response.
		after, after,
	}
	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	if fx.store.current != nil || fx.store.archiveResult != "completed" ||
		!equalUint64(fx.repo.completedIDs, []uint64{1, 2}) {
		t.Fatalf("unknown applied payout current=%#v archive=%q", fx.store.current, fx.store.archiveResult)
	}
	report := onlyReport(t, fx.notifier)
	if report.target(PayoutKindReferral).Payout.ConfirmationEvidence != "settlement_balance_decreased" {
		t.Fatalf("payout report = %#v", report.target(PayoutKindReferral).Payout)
	}
}

func TestExplicitlyRejectedPayoutIsArchivedAndCleared(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("1.5")
	fx.chain.payoutResult = exchange.SubmitResult{
		Rejected: true, Response: json.RawMessage("{\"status\":\"err\"}"),
	}
	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || !IsFatal(err) || fx.store.current != nil || fx.store.archiveResult != "rejected" {
		t.Fatalf("rejected payout error=%v current=%#v archive=%q", err, fx.store.current, fx.store.archiveResult)
	}
	report := onlyReport(t, fx.notifier)
	if stage := report.stage("finalize"); stage == nil || stage.Status != ReportStepSuccess {
		t.Fatalf("rejected finalize stage = %#v", stage)
	}
}

func TestPayoutIsNotSubmittedUntilBothDurableBoundariesSucceed(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("1.5")
	fx.store.failPhase = PhasePayoutSubmitting
	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil {
		t.Fatal("Run() error = nil")
	}
	if containsString(fx.chain.events, "submit:spotSend:0xsettlement") {
		t.Fatalf("payout submitted after state save failure: %v", fx.chain.events)
	}
	if fx.store.current == nil ||
		testActiveTarget(t, fx.store.current).Phase != PhasePayoutSubmitting ||
		testActiveTarget(t, fx.store.current).Payout == nil {
		t.Fatalf("durable current = %#v", fx.store.current)
	}
}

func TestPayoutRecoveryNeverResubmitsUncertainState(t *testing.T) {
	t.Run("submitting observes total decrease", func(t *testing.T) {
		fx := newFixture(t)
		state := fx.positiveState(t, PhasePayoutSubmitting)
		testTarget(t, &state, PayoutKindReferral).Payout = &PayoutJournal{
			Prepared: validPayout(t, state, 900), TotalBefore: "2", SubmitAttempts: 1,
		}
		fx.store.current = &state
		before := info.SpotBalanceAmounts{Total: decimal.NewFromInt(2), Available: decimal.NewFromInt(2)}
		after := info.SpotBalanceAmounts{
			Total: decimal.RequireFromString("0.5"), Available: decimal.RequireFromString("0.5"),
		}
		fx.chain.balanceSequence["0xsettlement"] = []info.SpotBalanceAmounts{before, after, after}
		fx.chain.balances["0xrecipient"] = decimal.RequireFromString("1.5")
		if err := fx.orchestrator.Recover(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(fx.chain.events) != 0 || fx.store.archiveResult != "completed" {
			t.Fatalf("recovery events=%v archive=%q", fx.chain.events, fx.store.archiveResult)
		}
	})

	t.Run("backup prepared is uncertain", func(t *testing.T) {
		fx := newFixture(t)
		state := fx.positiveState(t, PhasePayoutPrepared)
		testTarget(t, &state, PayoutKindReferral).Payout = &PayoutJournal{
			Prepared: validPayout(t, state, 901), TotalBefore: "2", SubmitAttempts: 1,
		}
		fx.store.current = &state
		fx.store.metadata = StateLoadMetadata{RecoveredFromBackup: true, PrimaryInvalid: true}
		fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
		if err := fx.orchestrator.Recover(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(fx.chain.submittedActions) != 1 ||
			!reflect.DeepEqual(fx.chain.submittedActions[0], validPayout(t, state, 901)) {
			t.Fatalf("backup recovery actions=%#v", fx.chain.submittedActions)
		}
	})

	t.Run("primary prepared submits persisted request", func(t *testing.T) {
		fx := newFixture(t)
		state := fx.positiveState(t, PhasePayoutPrepared)
		testTarget(t, &state, PayoutKindReferral).Payout = &PayoutJournal{
			Prepared: validPayout(t, state, 902), TotalBefore: "2", SubmitAttempts: 1,
		}
		fx.store.current = &state
		fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
		if err := fx.orchestrator.Recover(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !equalStrings(fx.chain.events, []string{"submit:spotSend:0xsettlement"}) {
			t.Fatalf("events = %v", fx.chain.events)
		}
		archivedTarget := testTarget(t, &fx.store.archived, PayoutKindReferral)
		if archivedTarget.Payout == nil || archivedTarget.Payout.SubmitAttempts != 1 {
			t.Fatalf("payout journal was not persisted: %#v", archivedTarget.Payout)
		}
	})
}

func TestConfirmedPayoutRecoveryOnlyCompletesDatabase(t *testing.T) {
	fx := newFixture(t)
	state := fx.positiveState(t, PhasePayoutConfirmed)
	testTarget(t, &state, PayoutKindReferral).Payout = &PayoutJournal{
		Prepared: validPayout(t, state, 903), TotalBefore: "2", SubmitAttempts: 1,
	}
	fx.store.current = &state
	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fx.chain.events) != 0 || !equalUint64(fx.repo.completedIDs, []uint64{1, 2}) {
		t.Fatalf("chain=%v completed=%v", fx.chain.events, fx.repo.completedIDs)
	}
}

func TestConfirmedPayoutRecoveryFailsClosedWithoutValidJournal(t *testing.T) {
	tests := []struct {
		name    string
		journal func(t *testing.T, state RunState) *PayoutJournal
	}{
		{name: "missing", journal: func(*testing.T, RunState) *PayoutJournal { return nil }},
		{name: "invalid settlement boundary", journal: func(t *testing.T, state RunState) *PayoutJournal {
			return &PayoutJournal{Prepared: validPayout(t, state, 905), TotalBefore: "bad", SubmitAttempts: 1}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fx := newFixture(t)
			state := fx.positiveState(t, PhasePayoutConfirmed)
			testTarget(t, &state, PayoutKindReferral).Payout = test.journal(t, state)
			fx.store.current = &state

			err := fx.orchestrator.Recover(context.Background())
			if err == nil || !IsFatal(err) || len(fx.repo.completedIDs) != 0 ||
				fx.store.current == nil {
				t.Fatalf("recovery error=%v completed=%v current=%#v",
					err, fx.repo.completedIDs, fx.store.current)
			}
		})
	}
}

func TestRecoveryUsesDailyConsolidationForPersistedUTCDate(t *testing.T) {
	fx := newFixture(t)
	state := fx.positiveState(t, PhasePayoutConfirmed)
	state.UTCDate = "2026-07-11"
	testTarget(t, &state, PayoutKindReferral).Payout = &PayoutJournal{
		Prepared: validPayout(t, state, 904), TotalBefore: "2", SubmitAttempts: 1,
	}
	fx.store.current = &state
	fx.store.consolidations = map[string]DailyConsolidation{
		"2026-07-11": {
			UTCDate: "2026-07-11", Status: DailyConsolidationCompleted,
			StartedAt:            time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC),
			ClaimablesCapturedAt: time.Date(2026, 7, 11, 0, 0, 1, 0, time.UTC),
			CompletedAt:          time.Date(2026, 7, 11, 0, 0, 2, 0, time.UTC),
			Builders: []DailyConsolidationBuilder{{
				Address: "0xbuilder", ClaimableUSDC: "1.6", ClaimableObserved: true,
				ClaimableQueryAttempts: 1,
				ClaimStatus:            ReportStepSuccess, SweepAmount: "1.6", SweepStatus: ReportStepSuccess,
			}},
		},
	}
	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	report := onlyReport(t, fx.notifier)
	if report.UTCDate != "2026-07-11" || report.DailyConsolidation.UTCDate != "2026-07-11" ||
		report.DailyConsolidation.TotalClaimableUSDC != "1.6" ||
		!report.DailyConsolidation.CoversPayout || report.Builders[0].ClaimableUSDC != "1.6" {
		t.Fatalf("recovery report = %#v", report)
	}
}

func TestRecordValidationFailureArchivesOnceAndIsFatal(t *testing.T) {
	fx := newFixture(t)
	fx.repo.records = []Record{
		{ID: 2, PeriodStartAt: 20, Amount: "1"},
		{ID: 1, PeriodStartAt: 10, Amount: "-0.1"},
	}
	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || !IsFatal(err) || fx.store.archiveResult != "failed_validation" ||
		fx.store.current != nil || len(fx.chain.events) != 0 {
		t.Fatalf("validation error=%v archive=%q current=%#v events=%v", err, fx.store.archiveResult, fx.store.current, fx.chain.events)
	}
}

func TestRunNoDataDoesNotCreatePayoutState(t *testing.T) {
	fx := newFixture(t)
	fx.repo.records = nil
	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	if len(fx.chain.events) != 0 || fx.store.archiveResult != "" || len(fx.store.saved) != 0 {
		t.Fatalf("no-data events=%v archive=%q saved=%d", fx.chain.events, fx.store.archiveResult, len(fx.store.saved))
	}
	report := onlyReport(t, fx.notifier)
	if report.Status != ReportStatusNoData || report.Outcome != "no_data" ||
		!report.RecordsRead || report.RecordCount != 0 || report.PayoutTotal != "0" ||
		!report.target(PayoutKindReferral).RecordsRead ||
		report.stage("finalize").Status != ReportStepSkipped {
		t.Fatalf("no-data report = %#v", report)
	}
}

func TestRateLimitObservationRunsAfterPayout(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("1.5")
	fx.chain.trackRateLimitUsage = true
	fx.chain.rateLimits["0xsettlement"] = info.UserRateLimit{NRequestsUsed: 800, NRequestsCap: 1000}
	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	if fx.chain.rateLimitCalls["0xsettlement"] != 1 {
		t.Fatalf("settlement rate limit calls = %d", fx.chain.rateLimitCalls["0xsettlement"])
	}
	if !containsString(fx.notifier.alerts, "user_rate_limit_low:0xsettlement") {
		t.Fatalf("alerts = %#v", fx.notifier.alerts)
	}
}

func TestNewOrchestratorRejectsUnsafeAccounts(t *testing.T) {
	fx := newFixture(t)
	for _, recipient := range []string{"0xbuilder", zeroEthereumAddress, "0xsettlement"} {
		t.Run(recipient, func(t *testing.T) {
			_, err := NewOrchestrator(OrchestratorConfig{
				ReferralRepository: fx.repo,
				Store:              fx.store, DailyConsolidations: fx.store,
				Chain: fx.chain, Builders: []string{"0xbuilder"},
				Settlement: "0xsettlement", ReferralRecipient: recipient,
				Clock: fx.clock, Nonce: &fakeNonce{next: 1}, Sleeper: fx.sleeper,
			})
			if err == nil {
				t.Fatalf("NewOrchestrator() accepted recipient %q", recipient)
			}
		})
	}
}

type fixture struct {
	orchestrator *Orchestrator
	repo         *fakeRepository
	store        *fakeStore
	chain        *fakeChain
	sleeper      *fakeSleeper
	notifier     *fakeNotifier
	logger       *fakeLogger
	clock        fixedClock
}

func newTwoBuilderOrchestrator(t *testing.T, fx *fixture) *Orchestrator {
	t.Helper()
	fx.chain.claimableRewards["0xbuilder-2"] = decimal.RequireFromString("1.25")
	fx.chain.balances["0xbuilder-2"] = decimal.Zero
	fx.chain.rateLimits["0xbuilder-2"] = info.UserRateLimit{NRequestsCap: 10_000}
	orchestrator, err := NewOrchestrator(OrchestratorConfig{
		ReferralRepository: fx.repo,
		Store:              fx.store, DailyConsolidations: fx.store, Chain: fx.chain,
		Notifier: fx.notifier, Logger: fx.logger,
		Builders:     []string{"0xbuilder", "0xbuilder-2"},
		BuilderNames: map[string]string{"0xbuilder": "builder-1", "0xbuilder-2": "builder-2"},
		Settlement:   "0xsettlement", ReferralRecipient: "0xrecipient",
		Clock: fx.clock, Nonce: &fakeNonce{next: uint64(fx.clock.now.UnixMilli())},
		Sleeper: fx.sleeper,
	})
	if err != nil {
		t.Fatal(err)
	}
	return orchestrator
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	repo := &fakeRepository{records: []Record{
		{ID: 1, PeriodStartAt: 10, Amount: "1"},
		{ID: 2, PeriodStartAt: 20, Amount: "0.5"},
	}}
	store := &fakeStore{}
	chain := &fakeChain{
		token: info.Token{Name: "USDC", TokenID: "0", WireToken: "USDC:0", WeiDecimals: 6},
		claimableRewards: map[string]decimal.Decimal{
			"0xbuilder": decimal.RequireFromString("1.5"),
		},
		claimableErrors:        map[string]error{},
		claimableErrorSequence: map[string][]error{},
		balances: map[string]decimal.Decimal{
			"0xbuilder": decimal.Zero, "0xsettlement": decimal.Zero,
		},
		holds:                map[string]decimal.Decimal{},
		balanceErrors:        map[string]error{},
		balanceErrorSequence: map[string][]error{},
		balanceSequence:      map[string][]info.SpotBalanceAmounts{},
		balanceCalls:         map[string]int{},
		rateLimits: map[string]info.UserRateLimit{
			"0xbuilder": {NRequestsCap: 10_000}, "0xsettlement": {NRequestsCap: 10_000},
		},
		rateLimitErrors: map[string]error{},
		rateLimitCalls:  map[string]int{},
		claimResult:     exchange.SubmitResult{Accepted: true},
		sweepResult:     exchange.SubmitResult{Accepted: true},
		payoutResult:    exchange.SubmitResult{Accepted: true},
	}
	sleeper := &fakeSleeper{}
	notifier := &fakeNotifier{}
	logger := &fakeLogger{}
	clock := fixedClock{now: time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)}
	orchestrator, err := NewOrchestrator(OrchestratorConfig{
		ReferralRepository: repo,
		Store:              store, DailyConsolidations: store,
		Chain: chain, Notifier: notifier, Logger: logger,
		Builders: []string{"0xbuilder"}, BuilderNames: map[string]string{"0xbuilder": "builder-1"},
		Settlement: "0xsettlement", ReferralRecipient: "0xrecipient", Network: "mainnet",
		Clock: clock, Nonce: &fakeNonce{next: uint64(clock.now.UnixMilli())}, Sleeper: sleeper,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{
		orchestrator: orchestrator, repo: repo,
		store: store, chain: chain,
		sleeper: sleeper, notifier: notifier, logger: logger, clock: clock,
	}
}

func (f *fixture) positiveState(t *testing.T, phase Phase) RunState {
	t.Helper()
	manifest, err := BuildManifest(ManifestInput{
		Records: f.repo.records, Token: &f.chain.token,
		Settlement: "0xsettlement", Recipient: "0xrecipient",
	})
	if err != nil {
		t.Fatal(err)
	}
	return RunState{
		RunID: "run", Trigger: TriggerUTC, UTCDate: "2026-07-12",
		Targets: []PayoutTargetState{
			{Kind: PayoutKindReferral, Phase: phase, Manifest: manifest},
		},
	}
}

type fakeRepository struct {
	records      []Record
	completedIDs []uint64
	listCalls    int
}

func (r *fakeRepository) ListPending(context.Context) ([]Record, error) {
	r.listCalls++
	return append([]Record(nil), r.records...), nil
}

func (r *fakeRepository) Complete(_ context.Context, ids []uint64) error {
	r.completedIDs = append([]uint64(nil), ids...)
	return nil
}

type fakeStore struct {
	current              *RunState
	saved                []RunState
	archived             RunState
	archiveResult        string
	failPhase            Phase
	metadata             StateLoadMetadata
	consolidations       map[string]DailyConsolidation
	consolidationSaves   []DailyConsolidation
	consolidationSaved   chan DailyConsolidation
	consolidationLoadErr error
	consolidationSaveErr error
}

func (s *fakeStore) LoadWithMetadata(context.Context) (*RunState, StateLoadMetadata, error) {
	return cloneState(s.current), s.metadata, nil
}

func (s *fakeStore) Save(_ context.Context, state RunState) error {
	copy := cloneState(&state)
	s.current = copy
	s.saved = append(s.saved, *cloneState(&state))
	active, _, _ := state.activePayoutTarget()
	if active != nil && active.Phase == s.failPhase && s.failPhase != "" {
		return errors.New("save failed")
	}
	return nil
}

func (s *fakeStore) Archive(_ context.Context, state RunState, result string) error {
	s.archived, s.archiveResult = *cloneState(&state), result
	return nil
}

func (s *fakeStore) Clear(context.Context) error {
	s.current = nil
	return nil
}

func (s *fakeStore) LoadDailyConsolidation(
	_ context.Context,
	utcDate string,
) (*DailyConsolidation, error) {
	if s.consolidationLoadErr != nil {
		return nil, s.consolidationLoadErr
	}
	record, exists := s.consolidations[utcDate]
	if !exists {
		return nil, nil
	}
	copy := record
	copy.Builders = append([]DailyConsolidationBuilder(nil), record.Builders...)
	return &copy, nil
}

func (s *fakeStore) SaveDailyConsolidation(
	_ context.Context,
	record DailyConsolidation,
) error {
	if s.consolidationSaveErr != nil {
		return s.consolidationSaveErr
	}
	if s.consolidations == nil {
		s.consolidations = make(map[string]DailyConsolidation)
	}
	copy := record
	copy.Builders = append([]DailyConsolidationBuilder(nil), record.Builders...)
	s.consolidations[record.UTCDate] = copy
	s.consolidationSaves = append(s.consolidationSaves, copy)
	if s.consolidationSaved != nil {
		s.consolidationSaved <- copy
	}
	return nil
}

type fakeChain struct {
	mu                     sync.Mutex
	token                  info.Token
	tokenError             error
	tokenCalls             int
	claimableRewards       map[string]decimal.Decimal
	claimableAfterClaim    map[string]decimal.Decimal
	claimableErrors        map[string]error
	claimableErrorSequence map[string][]error
	claimableCalls         map[string]int
	claimableStarted       chan string
	claimableRelease       chan struct{}
	balances               map[string]decimal.Decimal
	holds                  map[string]decimal.Decimal
	balanceErrors          map[string]error
	balanceErrorSequence   map[string][]error
	balanceSequence        map[string][]info.SpotBalanceAmounts
	balanceCalls           map[string]int
	rateLimits             map[string]info.UserRateLimit
	rateLimitErrors        map[string]error
	rateLimitCalls         map[string]int
	trackRateLimitUsage    bool
	claimPrepareErr        error
	claimResult            exchange.SubmitResult
	claimSubmitErr         error
	claimResults           map[string][]exchange.SubmitResult
	claimStarted           chan string
	claimRelease           chan struct{}
	sweepPrepareErr        error
	sweepResult            exchange.SubmitResult
	sweepSubmitErr         error
	sweepResults           map[string][]exchange.SubmitResult
	applyUnknownSweep      bool
	applyUnknownClaim      bool
	payoutResult           exchange.SubmitResult
	payoutResults          map[string][]exchange.SubmitResult
	applyUnknownPayout     bool
	events                 []string
	submittedActions       []exchange.PreparedAction
}

func (c *fakeChain) CanonicalUSDC(context.Context) (info.Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokenCalls++
	return c.token, c.tokenError
}

func (c *fakeChain) ClaimableUSDC(
	ctx context.Context,
	address string,
	_ info.Token,
) (decimal.Decimal, error) {
	c.mu.Lock()
	if c.claimableCalls == nil {
		c.claimableCalls = make(map[string]int)
	}
	c.claimableCalls[address]++
	var err error
	if sequence := c.claimableErrorSequence[address]; len(sequence) > 0 {
		err = sequence[0]
		c.claimableErrorSequence[address] = sequence[1:]
	} else {
		err = c.claimableErrors[address]
	}
	amount := c.claimableRewards[address]
	started, release := c.claimableStarted, c.claimableRelease
	c.mu.Unlock()
	if started != nil {
		select {
		case started <- address:
		case <-ctx.Done():
			return decimal.Zero, ctx.Err()
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return decimal.Zero, ctx.Err()
		}
	}
	if err != nil {
		return decimal.Zero, err
	}
	return amount, nil
}

func (c *fakeChain) SpotBalance(
	_ context.Context,
	address string,
	_ info.Token,
) (info.SpotBalanceAmounts, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.balanceCalls[address]++
	if sequence := c.balanceErrorSequence[address]; len(sequence) > 0 {
		err := sequence[0]
		c.balanceErrorSequence[address] = sequence[1:]
		if err != nil {
			return info.SpotBalanceAmounts{}, err
		}
	}
	if err := c.balanceErrors[address]; err != nil {
		return info.SpotBalanceAmounts{}, err
	}
	if sequence := c.balanceSequence[address]; len(sequence) > 0 {
		balance := sequence[0]
		c.balanceSequence[address] = sequence[1:]
		return balance, nil
	}
	return info.SpotBalanceAmounts{
		Total: c.balances[address], Available: c.balances[address].Sub(c.holds[address]),
	}, nil
}

func (c *fakeChain) UserRateLimit(
	_ context.Context,
	address string,
) (info.UserRateLimit, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rateLimitCalls[address]++
	if err := c.rateLimitErrors[address]; err != nil {
		return info.UserRateLimit{}, err
	}
	return c.rateLimits[address], nil
}

func (c *fakeChain) PrepareClaim(address string, nonce uint64) (exchange.PreparedAction, error) {
	if c.claimPrepareErr != nil {
		return exchange.PreparedAction{}, c.claimPrepareErr
	}
	return preparedAction(tinyTB{}, "claimRewards", address, "", "", "", nonce), nil
}

func (c *fakeChain) PrepareSpotSend(
	address, destination string,
	token info.Token,
	amount decimal.Decimal,
	nonce uint64,
) (exchange.PreparedAction, error) {
	if c.sweepPrepareErr != nil {
		return exchange.PreparedAction{}, c.sweepPrepareErr
	}
	return preparedAction(
		tinyTB{}, "spotSend", address, destination, token.WireToken, amount.String(), nonce,
	), nil
}

func (c *fakeChain) Submit(
	ctx context.Context,
	action exchange.PreparedAction,
) (exchange.SubmitResult, error) {
	if action.Kind == "claimRewards" {
		c.mu.Lock()
		started, release := c.claimStarted, c.claimRelease
		c.mu.Unlock()
		if started != nil {
			select {
			case started <- action.Signer:
			case <-ctx.Done():
				return exchange.SubmitResult{}, ctx.Err()
			}
		}
		if release != nil {
			select {
			case <-release:
			case <-ctx.Done():
				return exchange.SubmitResult{}, ctx.Err()
			}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, "submit:"+action.Kind+":"+action.Signer)
	c.submittedActions = append(c.submittedActions, clonePrepared(action))
	if action.Kind == "claimRewards" {
		result := c.claimResult
		if sequence := c.claimResults[action.Signer]; len(sequence) > 0 {
			result = sequence[0]
			c.claimResults[action.Signer] = sequence[1:]
		}
		if (c.claimSubmitErr == nil && result.Accepted) ||
			(c.applyUnknownClaim && (c.claimSubmitErr != nil || (!result.Accepted && !result.Rejected))) {
			reward := c.claimableRewards[action.Signer]
			c.balances[action.Signer] = c.balances[action.Signer].Add(reward)
			c.claimableRewards[action.Signer] = decimal.Zero
			if refill, exists := c.claimableAfterClaim[action.Signer]; exists {
				c.claimableRewards[action.Signer] = refill
			}
			c.incrementRateLimitUsageLocked(action.Signer)
		}
		return result, c.claimSubmitErr
	}
	amount := decimal.RequireFromString(action.Amount)
	if action.Signer == "0xsettlement" {
		result := c.payoutResult
		if sequence := c.payoutResults[action.Destination]; len(sequence) > 0 {
			result = sequence[0]
			c.payoutResults[action.Destination] = sequence[1:]
		}
		if result.Accepted || c.applyUnknownPayout {
			c.balances[action.Signer] = c.balances[action.Signer].Sub(amount)
			c.balances[action.Destination] = c.balances[action.Destination].Add(amount)
			c.incrementRateLimitUsageLocked(action.Signer)
		}
		return result, nil
	}
	available := c.balances[action.Signer].Sub(c.holds[action.Signer])
	if amount.GreaterThan(available) {
		return exchange.SubmitResult{Rejected: true}, nil
	}
	result := c.sweepResult
	if sequence := c.sweepResults[action.Signer]; len(sequence) > 0 {
		result = sequence[0]
		c.sweepResults[action.Signer] = sequence[1:]
	}
	if (c.sweepSubmitErr == nil && result.Accepted) ||
		(c.applyUnknownSweep && (c.sweepSubmitErr != nil || (!result.Accepted && !result.Rejected))) {
		c.balances[action.Signer] = c.balances[action.Signer].Sub(amount)
		c.balances[action.Destination] = c.balances[action.Destination].Add(amount)
		c.incrementRateLimitUsageLocked(action.Signer)
	}
	return result, c.sweepSubmitErr
}

func (c *fakeChain) incrementRateLimitUsageLocked(address string) {
	if !c.trackRateLimitUsage {
		return
	}
	limit := c.rateLimits[address]
	limit.NRequestsUsed++
	c.rateLimits[address] = limit
}

type fakeSleeper struct{ delays []time.Duration }

func (s *fakeSleeper) Sleep(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.delays = append(s.delays, delay)
	return nil
}

type fakeNotifier struct {
	alerts        []string
	alertSeverity []AlertSeverity
	alertMessages []string
	reports       []RunReport
}

func (n *fakeNotifier) Alert(
	_ context.Context,
	key string,
	severity AlertSeverity,
	message string,
) {
	n.alerts = append(n.alerts, key)
	n.alertSeverity = append(n.alertSeverity, severity)
	n.alertMessages = append(n.alertMessages, message)
}

func (n *fakeNotifier) Report(_ context.Context, report RunReport) {
	n.reports = append(n.reports, report)
}

type fakeLogEntry struct {
	level string
	attrs map[string]any
}

type fakeLogger struct{ entries []fakeLogEntry }

func (l *fakeLogger) Info(_ context.Context, _ string, attrs ...slog.Attr) {
	l.record("info", attrs)
}

func (l *fakeLogger) Warn(_ context.Context, _ string, attrs ...slog.Attr) {
	l.record("warn", attrs)
}

func (l *fakeLogger) Error(_ context.Context, _ string, attrs ...slog.Attr) {
	l.record("error", attrs)
}

func (l *fakeLogger) record(level string, attrs []slog.Attr) {
	values := make(map[string]any, len(attrs))
	for _, attr := range attrs {
		values[attr.Key] = attr.Value.Any()
	}
	l.entries = append(l.entries, fakeLogEntry{level: level, attrs: values})
}

func assertLogFields(t *testing.T, logger *fakeLogger, event string, want map[string]any) {
	t.Helper()
	for _, entry := range logger.entries {
		if entry.attrs["event"] != event {
			continue
		}
		for key, value := range want {
			if got := entry.attrs[key]; got != value {
				t.Fatalf("event %s field %s = %#v, want %#v", event, key, got, value)
			}
		}
		return
	}
	t.Fatalf("event %s was not logged", event)
}

func containsLogEvent(logger *fakeLogger, event string) bool {
	for _, entry := range logger.entries {
		if entry.attrs["event"] == event {
			return true
		}
	}
	return false
}

func containsLogFields(logger *fakeLogger, event string, fields map[string]any) bool {
	for _, entry := range logger.entries {
		if entry.attrs["event"] != event {
			continue
		}
		matches := true
		for key, want := range fields {
			if entry.attrs[key] != want {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type fakeNonce struct{ next uint64 }

func (n *fakeNonce) Next() uint64 {
	n.next++
	return n.next
}

type tinyTB struct{}

func (tinyTB) Helper()           {}
func (tinyTB) Fatal(args ...any) { panic(fmt.Sprint(args...)) }

type testTB interface {
	Helper()
	Fatal(...any)
}

func preparedAction(
	t testTB,
	kind, signer, destination, token, amount string,
	nonce uint64,
) exchange.PreparedAction {
	t.Helper()
	body := map[string]any{"action": map[string]any{"type": kind}, "nonce": nonce}
	if kind == "spotSend" {
		body["action"] = map[string]any{
			"type": kind, "destination": destination, "token": token,
			"amount": amount, "time": nonce,
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	return exchange.PreparedAction{
		Kind: kind, Signer: signer, Destination: destination, Token: token, Amount: amount,
		Nonce: nonce, RequestBody: raw, RequestHash: hex.EncodeToString(digest[:]),
	}
}

func validPayout(t *testing.T, state RunState, nonce uint64) exchange.PreparedAction {
	t.Helper()
	target := testTarget(t, &state, PayoutKindReferral)
	return preparedAction(
		t, "spotSend", target.Manifest.Settlement, target.Manifest.Recipient,
		target.Manifest.Token.WireToken, target.Manifest.PayoutTotal, nonce,
	)
}

func testTarget(t *testing.T, state *RunState, kind PayoutKind) *PayoutTargetState {
	t.Helper()
	if state == nil {
		t.Fatal("run state is nil")
	}
	for index := range state.Targets {
		if state.Targets[index].Kind == kind {
			return &state.Targets[index]
		}
	}
	t.Fatalf("run state has no %s target", kind)
	return nil
}

func testActiveTarget(t *testing.T, state *RunState) *PayoutTargetState {
	t.Helper()
	target, _, err := state.activePayoutTarget()
	if err != nil {
		t.Fatal(err)
	}
	if target == nil {
		t.Fatal("run state has no active payout target")
	}
	return target
}

func cloneState(state *RunState) *RunState {
	if state == nil {
		return nil
	}
	raw, _ := json.Marshal(state)
	var copy RunState
	_ = json.Unmarshal(raw, &copy)
	return &copy
}

func onlyReport(t *testing.T, notifier *fakeNotifier) RunReport {
	t.Helper()
	if len(notifier.reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(notifier.reports))
	}
	return notifier.reports[0]
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, value := range left {
		counts[value]++
	}
	for _, value := range right {
		counts[value]--
		if counts[value] < 0 {
			return false
		}
	}
	return true
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

func assertDistinctSweepNonces(
	t *testing.T,
	actions []exchange.PreparedAction,
	builder string,
	want int,
) {
	t.Helper()
	filtered := sweepActionsFor(actions, builder)
	seen := make(map[uint64]struct{}, want)
	for _, action := range filtered {
		if _, duplicate := seen[action.Nonce]; duplicate {
			t.Fatalf("sweep nonce %d was reused for %s", action.Nonce, builder)
		}
		seen[action.Nonce] = struct{}{}
	}
	if len(filtered) != want {
		t.Fatalf("sweep actions for %s = %d, want %d", builder, len(filtered), want)
	}
}

func sweepActionsFor(
	actions []exchange.PreparedAction,
	builder string,
) []exchange.PreparedAction {
	filtered := make([]exchange.PreparedAction, 0, len(actions))
	for _, action := range actions {
		if action.Kind == "spotSend" && strings.EqualFold(action.Signer, builder) {
			filtered = append(filtered, action)
		}
	}
	return filtered
}

func receiveWithin[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for concurrent operation")
		var zero T
		return zero
	}
}

func equalUint64(left, right []uint64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func countDelay(values []time.Duration, want time.Duration) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}
