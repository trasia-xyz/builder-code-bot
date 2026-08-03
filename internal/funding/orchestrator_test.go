package funding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	if report.Status != ReportStatusSuccess || report.Payout.Status != ReportStepSuccess ||
		!report.DailyConsolidation.Observed ||
		report.DailyConsolidation.TotalClaimableUSDC != "1.5" ||
		!report.DailyConsolidation.CoversPayout ||
		report.Builders[0].ClaimableUSDC != "1.5" ||
		report.Builders[0].SweepAmount != "1.5" {
		t.Fatalf("run report = %#v", report)
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

func TestExhaustedClaimableQueryMarksDailyReconciliationIncomplete(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimableErrors["0xbuilder"] = errors.New("info unavailable")
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	record := fx.store.consolidations["2026-07-12"]
	if len(record.Builders) != 1 || record.Builders[0].ClaimableObserved ||
		record.Builders[0].ClaimableQueryAttempts != claimableQueryAttempts ||
		record.Builders[0].ClaimAttempts != 1 ||
		record.Builders[0].ClaimStatus != ReportStepSuccess ||
		!containsString(fx.chain.events, "submit:claimRewards:0xbuilder") {
		t.Fatalf("incomplete daily record = %#v, events = %v", record, fx.chain.events)
	}

	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("1.5")
	fx.orchestrator.clock = fixedClock{now: time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)}
	if err := fx.orchestrator.RunScheduled(context.Background(), TriggerUTC, 1, 6); err != nil {
		t.Fatal(err)
	}
	report := onlyReport(t, fx.notifier)
	if report.DailyConsolidation.ObservedBuilders != 0 ||
		report.DailyConsolidation.CoversPayout || report.DailyConsolidation.BelowPayout ||
		!containsString(report.Warnings, "UTC 00:00 Claimable 仅成功查询 0/1 个 Builder，当日对账数据不完整") {
		t.Fatalf("incomplete reconciliation report = %#v", report)
	}
}

func TestUncertainClaimRetriesUntilAccepted(t *testing.T) {
	for _, test := range []struct {
		name     string
		results  []exchange.SubmitResult
		attempts int
	}{
		{name: "accepted after unknown", results: []exchange.SubmitResult{{}, {Accepted: true}}, attempts: 2},
		{
			name:     "rejection does not resolve earlier unknown",
			results:  []exchange.SubmitResult{{}, {Rejected: true}, {Accepted: true}},
			attempts: 3,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.chain.claimResults = map[string][]exchange.SubmitResult{"0xbuilder": test.results}
			if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
				t.Fatal(err)
			}
			builder := fx.store.consolidations["2026-07-12"].Builders[0]
			wantSummary := fmt.Sprintf("Reward 已领取（第 %d 次尝试）", test.attempts)
			if builder.ClaimAttempts != test.attempts || builder.ClaimUncertain ||
				builder.ClaimStatus != ReportStepSuccess || builder.ClaimSummary != wantSummary ||
				countString(fx.chain.events, "submit:claimRewards:0xbuilder") != test.attempts ||
				countDelay(fx.sleeper.delays, claimSubmitRetryInterval) != test.attempts-1 {
				t.Fatalf("retried claim builder = %#v, events=%v delays=%v", builder, fx.chain.events, fx.sleeper.delays)
			}
		})
	}
}

func TestUncertainSweepRetriesUntilAccepted(t *testing.T) {
	for _, test := range []struct {
		name     string
		results  []exchange.SubmitResult
		attempts int
		summary  string
	}{
		{
			name:     "accepted after unknown",
			results:  []exchange.SubmitResult{{}, {Accepted: true}},
			attempts: 2,
			summary:  "资金已归集（第 2 次尝试）",
		},
		{
			name:     "rejection does not resolve earlier unknown",
			results:  []exchange.SubmitResult{{}, {Rejected: true}, {Accepted: true}},
			attempts: 3,
			summary:  "资金已归集（第 3 次尝试）",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.chain.sweepResults = map[string][]exchange.SubmitResult{"0xbuilder": test.results}
			if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
				t.Fatal(err)
			}
			builder := fx.store.consolidations["2026-07-12"].Builders[0]
			if builder.SweepAmount != "1.5" || builder.SweepStatus != ReportStepSuccess ||
				builder.SweepSummary != test.summary ||
				countString(fx.chain.events, "submit:spotSend:0xbuilder") != test.attempts ||
				countDelay(fx.sleeper.delays, sweepSubmitRetryInterval) != test.attempts-1 {
				t.Fatalf("retried sweep builder = %#v, events=%v delays=%v", builder, fx.chain.events, fx.sleeper.delays)
			}
			if containsString(fx.chain.events, "submit:spotSend:0xsettlement") {
				t.Fatal("sweep retry path must not submit settlement payout")
			}
		})
	}
}

func TestUncertainSweepConfirmsWhenAvailableClears(t *testing.T) {
	fx := newFixture(t)
	fx.chain.sweepResults = map[string][]exchange.SubmitResult{"0xbuilder": {{}}}
	fx.chain.applyUnknownSweep = true
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.SweepAmount != "1.5" || builder.SweepStatus != ReportStepSuccess ||
		builder.SweepSummary != "资金已归集（余额已清空；此前提交结果不确定）" ||
		countString(fx.chain.events, "submit:spotSend:0xbuilder") != 1 ||
		countDelay(fx.sleeper.delays, sweepSubmitRetryInterval) != 1 {
		t.Fatalf("balance-cleared sweep = %#v, events=%v delays=%v", builder, fx.chain.events, fx.sleeper.delays)
	}
	if containsString(fx.chain.events, "submit:spotSend:0xsettlement") {
		t.Fatal("sweep confirmation path must not submit settlement payout")
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
		countString(fx.chain.events, "submit:spotSend:0xbuilder") != 1 ||
		countDelay(fx.sleeper.delays, sweepSubmitRetryInterval) != 0 {
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
	if fx.store.current == nil || fx.store.current.Phase != PhasePrepared {
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

func TestClaimableBelowPayoutWarnsWithoutBlockingFundedPayout(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimableRewards["0xbuilder"] = decimal.RequireFromString("1.4")
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("1.5")
	fx.orchestrator.clock = fixedClock{now: time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)}
	if err := fx.orchestrator.RunScheduled(context.Background(), TriggerUTC, 1, 6); err != nil {
		t.Fatal(err)
	}
	report := onlyReport(t, fx.notifier)
	warning := "UTC 00:00 Claimable 合计 1.4 USDC，低于数据库 Payout 1.5 USDC，短缺 0.1 USDC，请人工核查结算口径"
	if report.Status != ReportStatusWarning || report.Payout.Status != ReportStepSuccess ||
		!report.DailyConsolidation.BelowPayout ||
		report.DailyConsolidation.ShortfallUSDC != "0.1" ||
		!containsString(report.Warnings, warning) {
		t.Fatalf("claimed reconciliation report = %#v", report)
	}
}

func TestUncertainClaimRetriesWithoutChangingClaimableReconciliation(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimResult = exchange.SubmitResult{}
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	record := fx.store.consolidations["2026-07-12"]
	if len(record.Builders) != 1 || !record.Builders[0].ClaimUncertain ||
		record.Builders[0].ClaimableUSDC != "1.5" ||
		record.Builders[0].ClaimAttempts != claimSubmitAttempts {
		t.Fatalf("uncertain daily consolidation = %#v", record)
	}
	fx.orchestrator.clock = fixedClock{now: time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)}
	if err := fx.orchestrator.RunScheduled(context.Background(), TriggerUTC, 1, 6); err != nil {
		t.Fatal(err)
	}
	report := onlyReport(t, fx.notifier)
	if report.DailyConsolidation.UncertainClaims != 1 ||
		!report.DailyConsolidation.CoversPayout || report.DailyConsolidation.BelowPayout ||
		!containsString(report.Warnings, "UTC 00:00 有 1 个 Builder 的 claim 在有限重试后结果仍不确定，请人工核查") {
		t.Fatalf("uncertain claim report = %#v", report)
	}
}

func TestClaimAtOrBelowThresholdIsRecordedAsZero(t *testing.T) {
	for _, amount := range []string{"0", "1"} {
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

func TestConsolidateOnlyDoesNotReadOrWriteDailyRecord(t *testing.T) {
	fx := newFixture(t)
	fx.store.current = &RunState{RunID: "existing", Phase: PhasePrepared}
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
	fx.chain.balances["0xbuilder-2"] = decimal.RequireFromString("1.25")
	orchestrator, err := NewOrchestrator(OrchestratorConfig{
		Repository: fx.repo, Store: fx.store, DailyConsolidations: fx.store, Chain: fx.chain,
		Notifier: fx.notifier, Logger: fx.logger,
		Builders:     []string{"0xbuilder", "0xbuilder-2"},
		BuilderNames: map[string]string{"0xbuilder": "builder-1", "0xbuilder-2": "builder-2"},
		Settlement:   "0xsettlement", Recipient: "0xrecipient",
		Clock: fx.clock, Nonce: &fakeNonce{next: uint64(fx.clock.now.UnixMilli())},
		Sleeper: fx.sleeper,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countDelay(fx.sleeper.delays, claimBalanceObservationInterval) != 1 {
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

func TestClaimVisibilityExhaustionStillRecordsBestEffortSweep(t *testing.T) {
	fx := newFixture(t)
	latest := info.SpotBalanceAmounts{
		Total: decimal.RequireFromString("2"), Available: decimal.RequireFromString("0.5"),
	}
	fx.chain.balanceSequence["0xbuilder"] = make([]info.SpotBalanceAmounts, claimBalanceObservationAttempts)
	for index := range fx.chain.balanceSequence["0xbuilder"] {
		fx.chain.balanceSequence["0xbuilder"][index] = latest
	}
	if err := fx.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	builder := fx.store.consolidations["2026-07-12"].Builders[0]
	if builder.SweepAmount != "0.5" || builder.SweepStatus != ReportStepWarning {
		t.Fatalf("daily builder = %#v", builder)
	}
	if fx.chain.balanceCalls["0xbuilder"] != claimBalanceObservationAttempts {
		t.Fatalf("balance calls = %d", fx.chain.balanceCalls["0xbuilder"])
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
		{Total: decimal.NewFromInt(2), Available: decimal.RequireFromString("0.4")},
		{Total: decimal.NewFromInt(2), Available: decimal.RequireFromString("0.3")},
		{Total: decimal.NewFromInt(2), Available: decimal.RequireFromString("0.2")},
		{Total: decimal.NewFromInt(2), Available: decimal.RequireFromString("0.1")},
		{Total: decimal.NewFromInt(2), Available: decimal.Zero},
	}
	err = fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || !IsFatal(err) || fx.store.current == nil || fx.store.current.Phase != PhaseBlocked {
		t.Fatalf("available-only decrease error=%v current=%#v", err, fx.store.current)
	}
}

func TestUnknownPayoutConfirmsOnlyAfterSettlementTotalDecreases(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("1.5")
	fx.chain.payoutResult = exchange.SubmitResult{}
	fx.chain.applyUnknownPayout = true
	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	if fx.store.current != nil || fx.store.archiveResult != "completed" ||
		!equalUint64(fx.repo.completedIDs, []uint64{1, 2}) {
		t.Fatalf("unknown applied payout current=%#v archive=%q", fx.store.current, fx.store.archiveResult)
	}
	report := onlyReport(t, fx.notifier)
	if report.Payout.ConfirmationEvidence != "settlement_balance_decreased" {
		t.Fatalf("payout report = %#v", report.Payout)
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
	if fx.store.current == nil || fx.store.current.Phase != PhasePayoutSubmitting ||
		fx.store.current.Payout == nil {
		t.Fatalf("durable current = %#v", fx.store.current)
	}
}

func TestPayoutRecoveryNeverResubmitsUncertainState(t *testing.T) {
	t.Run("submitting observes total decrease", func(t *testing.T) {
		fx := newFixture(t)
		state := fx.positiveState(t, PhasePayoutSubmitting)
		state.Payout = &PayoutJournal{Prepared: validPayout(t, state, 900), TotalBefore: "2"}
		fx.store.current = &state
		fx.chain.balances["0xsettlement"] = decimal.RequireFromString("0.5")
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
		state.Payout = &PayoutJournal{Prepared: validPayout(t, state, 901), TotalBefore: "2"}
		fx.store.current = &state
		fx.store.metadata = StateLoadMetadata{RecoveredFromBackup: true, PrimaryInvalid: true}
		fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
		err := fx.orchestrator.Recover(context.Background())
		if err == nil || !IsFatal(err) || len(fx.chain.events) != 0 {
			t.Fatalf("backup recovery error=%v events=%v", err, fx.chain.events)
		}
	})

	t.Run("primary prepared submits persisted request", func(t *testing.T) {
		fx := newFixture(t)
		state := fx.positiveState(t, PhasePayoutPrepared)
		state.Payout = &PayoutJournal{Prepared: validPayout(t, state, 902), TotalBefore: "2"}
		fx.store.current = &state
		fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
		if err := fx.orchestrator.Recover(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !equalStrings(fx.chain.events, []string{"submit:spotSend:0xsettlement"}) {
			t.Fatalf("events = %v", fx.chain.events)
		}
	})
}

func TestConfirmedPayoutRecoveryOnlyCompletesDatabase(t *testing.T) {
	fx := newFixture(t)
	state := fx.positiveState(t, PhasePayoutConfirmed)
	state.Payout = &PayoutJournal{Prepared: validPayout(t, state, 903), TotalBefore: "2"}
	fx.store.current = &state
	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fx.chain.events) != 0 || !equalUint64(fx.repo.completedIDs, []uint64{1, 2}) {
		t.Fatalf("chain=%v completed=%v", fx.chain.events, fx.repo.completedIDs)
	}
}

func TestRecoveryUsesDailyConsolidationForPersistedUTCDate(t *testing.T) {
	fx := newFixture(t)
	state := fx.positiveState(t, PhasePayoutConfirmed)
	state.UTCDate = "2026-07-11"
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
	if report.Status != ReportStatusNoData || report.Outcome != "no_data" {
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
				Repository: fx.repo, Store: fx.store, DailyConsolidations: fx.store,
				Chain: fx.chain, Builders: []string{"0xbuilder"},
				Settlement: "0xsettlement", Recipient: recipient,
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
	fx.chain.balances["0xbuilder-2"] = decimal.RequireFromString("1.25")
	fx.chain.rateLimits["0xbuilder-2"] = info.UserRateLimit{NRequestsCap: 10_000}
	orchestrator, err := NewOrchestrator(OrchestratorConfig{
		Repository: fx.repo, Store: fx.store, DailyConsolidations: fx.store, Chain: fx.chain,
		Notifier: fx.notifier, Logger: fx.logger,
		Builders:     []string{"0xbuilder", "0xbuilder-2"},
		BuilderNames: map[string]string{"0xbuilder": "builder-1", "0xbuilder-2": "builder-2"},
		Settlement:   "0xsettlement", Recipient: "0xrecipient",
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
			"0xbuilder": decimal.RequireFromString("1.5"), "0xsettlement": decimal.Zero,
		},
		holds:           map[string]decimal.Decimal{},
		balanceErrors:   map[string]error{},
		balanceSequence: map[string][]info.SpotBalanceAmounts{},
		balanceCalls:    map[string]int{},
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
		Repository: repo, Store: store, DailyConsolidations: store,
		Chain: chain, Notifier: notifier, Logger: logger,
		Builders: []string{"0xbuilder"}, BuilderNames: map[string]string{"0xbuilder": "builder-1"},
		Settlement: "0xsettlement", Recipient: "0xrecipient", Network: "mainnet",
		Clock: clock, Nonce: &fakeNonce{next: uint64(clock.now.UnixMilli())}, Sleeper: sleeper,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{
		orchestrator: orchestrator, repo: repo, store: store, chain: chain,
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
		Phase: phase, Manifest: manifest,
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
	if state.Phase == s.failPhase && s.failPhase != "" {
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
	claimableErrors        map[string]error
	claimableErrorSequence map[string][]error
	claimableCalls         map[string]int
	claimableStarted       chan string
	claimableRelease       chan struct{}
	balances               map[string]decimal.Decimal
	holds                  map[string]decimal.Decimal
	balanceErrors          map[string]error
	balanceSequence        map[string][]info.SpotBalanceAmounts
	balanceCalls           map[string]int
	rateLimits             map[string]info.UserRateLimit
	rateLimitErrors        map[string]error
	rateLimitCalls         map[string]int
	trackRateLimitUsage    bool
	claimResult            exchange.SubmitResult
	claimResults           map[string][]exchange.SubmitResult
	claimStarted           chan string
	claimRelease           chan struct{}
	sweepResult            exchange.SubmitResult
	sweepResults           map[string][]exchange.SubmitResult
	applyUnknownSweep      bool
	payoutResult           exchange.SubmitResult
	applyUnknownPayout     bool
	events                 []string
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
	return preparedAction(tinyTB{}, "claimRewards", address, "", "", "", nonce), nil
}

func (c *fakeChain) PrepareSpotSend(
	address, destination string,
	token info.Token,
	amount decimal.Decimal,
	nonce uint64,
) (exchange.PreparedAction, error) {
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
	if c.trackRateLimitUsage {
		limit := c.rateLimits[action.Signer]
		limit.NRequestsUsed++
		c.rateLimits[action.Signer] = limit
	}
	if action.Kind == "claimRewards" {
		if sequence := c.claimResults[action.Signer]; len(sequence) > 0 {
			result := sequence[0]
			c.claimResults[action.Signer] = sequence[1:]
			return result, nil
		}
		return c.claimResult, nil
	}
	amount := decimal.RequireFromString(action.Amount)
	if action.Signer == "0xsettlement" {
		if c.payoutResult.Accepted || c.applyUnknownPayout {
			c.balances[action.Signer] = c.balances[action.Signer].Sub(amount)
		}
		return c.payoutResult, nil
	}
	result := c.sweepResult
	if sequence := c.sweepResults[action.Signer]; len(sequence) > 0 {
		result = sequence[0]
		c.sweepResults[action.Signer] = sequence[1:]
	}
	if result.Accepted || (c.applyUnknownSweep && !result.Accepted && !result.Rejected) {
		c.balances[action.Signer] = c.balances[action.Signer].Sub(amount)
		c.balances[action.Destination] = c.balances[action.Destination].Add(amount)
	}
	return result, nil
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
	return preparedAction(
		t, "spotSend", state.Manifest.Settlement, state.Manifest.Recipient,
		state.Manifest.Token.WireToken, state.Manifest.PayoutTotal, nonce,
	)
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
