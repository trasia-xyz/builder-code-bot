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
	"testing"
	"time"

	"builder-code-bot/internal/hyperliquid/exchange"
	"builder-code-bot/internal/hyperliquid/info"

	"github.com/shopspring/decimal"
)

func TestRunHappyPathOnlyPersistsFinalPayout(t *testing.T) {
	fx := newFixture(t)
	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	if got := fx.chain.events; !equalStrings(got, []string{
		"submit:claimRewards:0xbuilder", "submit:spotSend:0xbuilder", "submit:spotSend:0xsettlement",
	}) {
		t.Fatalf("chain events = %v", got)
	}
	if !equalUint64(fx.repo.completedIDs, []uint64{1, 2}) {
		t.Fatalf("completed IDs = %v", fx.repo.completedIDs)
	}
	if fx.store.current != nil || fx.store.archiveResult != "completed" {
		t.Fatalf("current = %#v, archive = %q", fx.store.current, fx.store.archiveResult)
	}
	if len(fx.sleeper.delays) != 0 {
		t.Fatalf("delays = %v, want immediate convergence", fx.sleeper.delays)
	}
	if got := fx.chain.claimableCalls["0xbuilder"]; got != 1 {
		t.Fatalf("ClaimableUSDC() calls = %d, want 1", got)
	}
	for _, saved := range fx.store.saved {
		if saved.Payout == nil && saved.Phase != PhasePrepared {
			t.Fatalf("unexpected state before payout journal: %#v", saved)
		}
	}
}

func TestRunLogsFundingBalancesAndAmounts(t *testing.T) {
	fx := newFixture(t)
	fx.chain.payoutResult.Response = json.RawMessage(`{"status":"ok"}`)
	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}

	assertLogFields(t, fx.logger, "funding_snapshot_calculated", map[string]any{
		"record_count": int64(2), "raw_total": "1.500000000000000000", "payout_total": "1.5",
	})
	assertLogFields(t, fx.logger, "funding_builder_claim_eligibility_checked", map[string]any{
		"builder": "0xbuilder", "claimable_usdc": "1.5", "threshold_usdc": "1", "eligible": true,
	})
	assertLogFields(t, fx.logger, "funding_builder_balance_observed", map[string]any{
		"builder": "0xbuilder", "attempt": int64(1),
		"total": "1.5", "hold": "0", "available": "1.5",
	})
	assertLogFields(t, fx.logger, "funding_settlement_balance_observed", map[string]any{
		"attempt": int64(1), "total": "1.5", "hold": "0", "available": "1.5",
		"payout_total": "1.5", "sufficient": true,
	})
	assertLogFields(t, fx.logger, "funding_payout_preparation_started", map[string]any{
		"payout_total": "1.5", "settlement_total_before": "1.5",
	})
	assertLogFields(t, fx.logger, "funding_payout_submit_result", map[string]any{
		"payout_total": "1.5", "settlement_total_before": "1.5",
		"accepted": true, "response": `{"status":"ok"}`,
	})
	assertLogFields(t, fx.logger, "funding_run_completed", map[string]any{
		"record_count": int64(2), "raw_total": "1.500000000000000000", "payout_total": "1.5",
	})
}

func TestClaimRewardIsSkippedWithoutAlertAtOrBelowThreshold(t *testing.T) {
	for _, amount := range []string{"0", "1"} {
		t.Run(amount, func(t *testing.T) {
			fx := newFixture(t)
			fx.chain.claimableRewards["0xbuilder"] = decimal.RequireFromString(amount)
			fx.chain.balances["0xbuilder"] = decimal.Zero
			fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)

			if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
				t.Fatal(err)
			}
			if containsString(fx.chain.events, "submit:claimRewards:0xbuilder") {
				t.Fatalf("claim was submitted with %s claimable USDC: %v", amount, fx.chain.events)
			}
			if len(fx.notifier.alerts) != 0 {
				t.Fatalf("alerts = %#v, want none", fx.notifier.alerts)
			}
			assertLogFields(t, fx.logger, "funding_builder_claim_eligibility_checked", map[string]any{
				"builder": "0xbuilder", "claimable_usdc": amount, "threshold_usdc": "1", "eligible": false,
			})
		})
	}
}

func TestClaimableRewardQueryFailureAlertsWithoutSubmittingClaim(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimableErrors["0xbuilder"] = errors.New("referral endpoint unavailable")
	fx.chain.balances["0xbuilder"] = decimal.Zero
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)

	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	if containsString(fx.chain.events, "submit:claimRewards:0xbuilder") {
		t.Fatalf("claim was submitted after eligibility query failure: %v", fx.chain.events)
	}
	if len(fx.notifier.alerts) != 1 {
		t.Fatalf("alerts = %#v, want one query failure alert", fx.notifier.alerts)
	}
	assertLogFields(t, fx.logger, "funding_builder_action_failed", map[string]any{
		"builder": "0xbuilder", "action_kind": "claimRewards", "outcome": "query",
	})
}

func TestRunObservesBuilderAndSettlementRateLimits(t *testing.T) {
	fx := newFixture(t)
	fx.chain.trackRateLimitUsage = true
	fx.chain.rateLimits["0xbuilder"] = info.UserRateLimit{
		NRequestsUsed: 798,
		NRequestsCap:  1000,
	}
	fx.chain.rateLimits["0xsettlement"] = info.UserRateLimit{
		NRequestsUsed: 800,
		NRequestsCap:  1000,
	}

	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"0xbuilder", "0xsettlement"} {
		if got := fx.chain.rateLimitCalls[address]; got != 1 {
			t.Fatalf("UserRateLimit(%s) calls = %d, want 1", address, got)
		}
	}
	assertRateLimitLog(t, fx.logger, "0xbuilder", "info", map[string]any{
		"account_kind": "builder", "requests_remaining": uint64(200),
	})
	assertRateLimitLog(t, fx.logger, "0xsettlement", "warn", map[string]any{
		"account_kind": "settlement", "requests_remaining": uint64(199),
	})
	if len(fx.notifier.alerts) != 1 || fx.notifier.alerts[0] != "user_rate_limit_low:0xsettlement" {
		t.Fatalf("alerts = %#v, want settlement rate limit alert", fx.notifier.alerts)
	}
	wantAlertMessage := "Hyperliquid user rate limit is low\naccount kind: settlement\naddress: 0xsettlement\nrequests remaining: 199"
	if fx.notifier.alertMessages[0] != wantAlertMessage {
		t.Fatalf("alert message = %q, want %q", fx.notifier.alertMessages[0], wantAlertMessage)
	}
}

func TestRunObservesRateLimitsAfterOrdinaryAndFatalFailures(t *testing.T) {
	for _, test := range []struct {
		name                    string
		configure               func(*fixture)
		wantFatal               bool
		wantBuilderRemaining    uint64
		wantSettlementRemaining uint64
	}{
		{
			name: "ordinary underfunded failure",
			configure: func(fx *fixture) {
				fx.chain.balances["0xbuilder"] = decimal.Zero
			},
			wantBuilderRemaining:    9_999,
			wantSettlementRemaining: 10_000,
		},
		{
			name: "fatal rejected payout",
			configure: func(fx *fixture) {
				fx.chain.payoutResult = exchange.SubmitResult{Rejected: true}
			},
			wantFatal:               true,
			wantBuilderRemaining:    9_998,
			wantSettlementRemaining: 9_999,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fx := newFixture(t)
			fx.chain.trackRateLimitUsage = true
			test.configure(fx)

			err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
			if err == nil || IsFatal(err) != test.wantFatal {
				t.Fatalf("Run() error = %v, fatal = %v, want fatal %v", err, IsFatal(err), test.wantFatal)
			}
			assertRateLimitLog(t, fx.logger, "0xbuilder", "info", map[string]any{
				"requests_remaining": test.wantBuilderRemaining,
			})
			assertRateLimitLog(t, fx.logger, "0xsettlement", "info", map[string]any{
				"requests_remaining": test.wantSettlementRemaining,
			})
		})
	}
}

func TestRunUsesUncanceledContextForFinalRateLimitObservation(t *testing.T) {
	fx := newFixture(t)
	fx.chain.respectRateLimitContext = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := fx.orchestrator.Run(ctx, TriggerRunOnStart)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
	assertRateLimitLog(t, fx.logger, "0xbuilder", "info", map[string]any{
		"requests_remaining": uint64(10_000),
	})
}

func TestRateLimitQueryFailureDoesNotBlockFunding(t *testing.T) {
	fx := newFixture(t)
	fx.chain.rateLimitErrors["0xbuilder"] = errors.New("unavailable")

	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	assertLogFields(t, fx.logger, "funding_user_rate_limit_query_failed", map[string]any{
		"account_kind": "builder", "address": "0xbuilder",
	})
	if fx.store.archiveResult != "completed" {
		t.Fatalf("archive = %q, want completed", fx.store.archiveResult)
	}
}

func TestRateLimitAlertIsLatchedUntilTheAccountRecovers(t *testing.T) {
	fx := newFixture(t)
	fx.repo.records = nil
	fx.chain.rateLimits["0xbuilder"] = info.UserRateLimit{NRequestsUsed: 9801, NRequestsCap: 10_000}

	for range 2 {
		if err := fx.orchestrator.Run(context.Background(), TriggerUTC); err != nil {
			t.Fatal(err)
		}
	}
	if len(fx.notifier.alerts) != 1 {
		t.Fatalf("alerts while continuously low = %#v, want one", fx.notifier.alerts)
	}

	fx.chain.rateLimits["0xbuilder"] = info.UserRateLimit{NRequestsUsed: 9800, NRequestsCap: 10_000}
	if err := fx.orchestrator.Run(context.Background(), TriggerUTC); err != nil {
		t.Fatal(err)
	}
	fx.chain.rateLimits["0xbuilder"] = info.UserRateLimit{NRequestsUsed: 9801, NRequestsCap: 10_000}
	if err := fx.orchestrator.Run(context.Background(), TriggerUTC); err != nil {
		t.Fatal(err)
	}
	if len(fx.notifier.alerts) != 2 {
		t.Fatalf("alerts after recovery and regression = %#v, want two", fx.notifier.alerts)
	}
}

func TestUnknownPayoutConfirmsWhenSettlementBalanceDecreases(t *testing.T) {
	fx := newFixture(t)
	fx.chain.payoutResult = exchange.SubmitResult{}
	fx.chain.applyUnknownPayout = true
	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	if fx.store.archiveResult != "completed" || len(fx.repo.completedIDs) == 0 {
		t.Fatalf("archive = %q, completed = %v", fx.store.archiveResult, fx.repo.completedIDs)
	}
}

func TestUnknownPayoutBlocksAfterFiniteBalanceObservations(t *testing.T) {
	fx := newFixture(t)
	fx.chain.payoutResult = exchange.SubmitResult{}
	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || !IsFatal(err) {
		t.Fatalf("Run() error = %v, want fatal", err)
	}
	if fx.store.current == nil || fx.store.current.Phase != PhaseBlocked || fx.store.archiveResult != "blocked" {
		t.Fatalf("current = %#v, archive = %q", fx.store.current, fx.store.archiveResult)
	}
	if got := countDelay(fx.sleeper.delays, payoutBalanceObservationInterval); got != payoutBalanceObservationAttempts-1 {
		t.Fatalf("one-second delays = %d, want bounded observations", got)
	}
	if len(fx.repo.completedIDs) != 0 {
		t.Fatalf("database completed after ambiguous payout: %v", fx.repo.completedIDs)
	}
	if got := fx.chain.balanceCalls["0xsettlement"]; got != 1+payoutBalanceObservationAttempts {
		t.Fatalf("settlement balance calls = %d, want sufficiency plus %d observations", got, payoutBalanceObservationAttempts)
	}
}

func TestUnknownPayoutIgnoresAvailableDecreaseWhenTotalIsUnchanged(t *testing.T) {
	fx := newFixture(t)
	fx.chain.payoutResult = exchange.SubmitResult{}
	fx.chain.balanceSequence["0xsettlement"] = []info.SpotBalanceAmounts{
		{Total: decimal.RequireFromString("1.5"), Available: decimal.RequireFromString("1.5")},
		{Total: decimal.RequireFromString("1.5"), Available: decimal.RequireFromString("0.5")},
	}
	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || !IsFatal(err) || fx.store.current == nil || fx.store.current.Phase != PhaseBlocked {
		t.Fatalf("Run() = %v, current = %#v", err, fx.store.current)
	}
	if len(fx.repo.completedIDs) != 0 {
		t.Fatalf("database completed after hold-only change: %v", fx.repo.completedIDs)
	}
}

func TestAvailableBalanceControlsBuilderSweepAndPayoutSufficiency(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xbuilder"] = decimal.NewFromInt(2)
	fx.chain.holds["0xbuilder"] = decimal.RequireFromString("0.5")
	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	if got := fx.chain.balances["0xbuilder"]; !got.Equal(decimal.RequireFromString("0.5")) {
		t.Fatalf("builder total after sweep = %s, want held 0.5", got)
	}

	fx = newFixture(t)
	fx.chain.balances["0xbuilder"] = decimal.Zero
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	fx.chain.holds["0xsettlement"] = decimal.NewFromInt(1)
	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || IsFatal(err) {
		t.Fatalf("Run() error = %v, want ordinary underfunded error", err)
	}
	if containsString(fx.chain.events, "submit:spotSend:0xsettlement") {
		t.Fatalf("payout used total instead of available: %v", fx.chain.events)
	}
}

func TestBuilderRewardDelayedVisibilityConvergesWithinFinitePolling(t *testing.T) {
	fx := newFixture(t)
	zero := info.SpotBalanceAmounts{Total: decimal.Zero, Available: decimal.Zero}
	fx.chain.balanceSequence["0xbuilder"] = []info.SpotBalanceAmounts{zero, zero}
	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	if got := countDelay(fx.sleeper.delays, builderConvergenceInterval); got != 2 {
		t.Fatalf("convergence delays = %d, want 2", got)
	}
	if fx.store.archiveResult != "completed" {
		t.Fatalf("archive = %q", fx.store.archiveResult)
	}
}

func TestBuilderConvergenceExhaustionIsOrdinaryError(t *testing.T) {
	fx := newFixture(t)
	fx.chain.balances["0xbuilder"] = decimal.Zero
	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || IsFatal(err) {
		t.Fatalf("Run() error = %v, want ordinary error", err)
	}
	if got := countDelay(fx.sleeper.delays, builderConvergenceInterval); got != builderConvergenceAttempts-1 {
		t.Fatalf("convergence delays = %d, want %d", got, builderConvergenceAttempts-1)
	}
}

func TestRecordValidationFailureArchivesOnceAndIsFatal(t *testing.T) {
	fx := newFixture(t)
	fx.repo.records = []Record{
		{ID: 2, PeriodStartAt: 20, Amount: "1"},
		{ID: 1, PeriodStartAt: 10, Amount: "-0.1"},
	}
	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || !IsFatal(err) {
		t.Fatalf("Run() error = %v, want fatal validation error", err)
	}
	if fx.store.archiveResult != "failed_validation" || fx.store.current != nil {
		t.Fatalf("archive = %q, current = %#v", fx.store.archiveResult, fx.store.current)
	}
	if got := fx.store.archived.Manifest.Records; len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("archived records = %#v", got)
	}
	if len(fx.chain.events) != 0 {
		t.Fatalf("chain events after validation failure = %v", fx.chain.events)
	}
}

func TestRejectedPayoutBlocksWithoutBalanceObservation(t *testing.T) {
	fx := newFixture(t)
	fx.chain.payoutResult = exchange.SubmitResult{Rejected: true}
	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil || !IsFatal(err) || fx.store.current != nil {
		t.Fatalf("error = %v, current = %#v", err, fx.store.current)
	}
	if fx.store.archiveResult != "rejected" {
		t.Fatalf("archive = %q", fx.store.archiveResult)
	}
}

func TestBuilderFailuresDoNotBlockPreFundedPayout(t *testing.T) {
	fx := newFixture(t)
	fx.chain.claimResult = exchange.SubmitResult{Rejected: true}
	fx.chain.balanceErrors["0xbuilder"] = errors.New("unavailable")
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	if fx.store.archiveResult != "completed" {
		t.Fatalf("archive = %q", fx.store.archiveResult)
	}
	if len(fx.notifier.alerts) == 0 {
		t.Fatal("builder failures did not alert")
	}
}

func TestRecoverSubmittingPayoutUsesBalanceWithoutResubmitting(t *testing.T) {
	fx := newFixture(t)
	state := fx.positiveState(t, PhasePayoutSubmitting)
	state.Payout = &PayoutJournal{
		Prepared: validPayout(t, state, 900), TotalBefore: "2",
	}
	fx.store.current = &state
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("0.5")
	fx.chain.events = nil
	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fx.chain.events) != 0 {
		t.Fatalf("recovery resubmitted chain action: %v", fx.chain.events)
	}
	if fx.store.archiveResult != "completed" {
		t.Fatalf("archive = %q", fx.store.archiveResult)
	}
}

func TestRecoverPrimaryPreparedPayoutSubmitsPersistedRequest(t *testing.T) {
	fx := newFixture(t)
	state := fx.positiveState(t, PhasePayoutPrepared)
	state.Payout = &PayoutJournal{Prepared: validPayout(t, state, 902), TotalBefore: "2"}
	fx.store.current = &state
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	fx.chain.events = nil
	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !equalStrings(fx.chain.events, []string{"submit:spotSend:0xsettlement"}) {
		t.Fatalf("events = %v", fx.chain.events)
	}
}

func TestRecoverBackupPreparedPayoutDoesNotResubmit(t *testing.T) {
	fx := newFixture(t)
	state := fx.positiveState(t, PhasePayoutPrepared)
	state.Payout = &PayoutJournal{Prepared: validPayout(t, state, 903), TotalBefore: "2"}
	fx.store.current = &state
	fx.store.metadata = StateLoadMetadata{RecoveredFromBackup: true, PrimaryInvalid: true}
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	fx.chain.events = nil
	err := fx.orchestrator.Recover(context.Background())
	if err == nil || !IsFatal(err) {
		t.Fatalf("Recover() error = %v, want fatal ambiguity", err)
	}
	if len(fx.chain.events) != 0 {
		t.Fatalf("backup recovery resubmitted payout: %v", fx.chain.events)
	}
}

func TestRecoverConfirmedPayoutOnlyCompletesDatabase(t *testing.T) {
	fx := newFixture(t)
	state := fx.positiveState(t, PhasePayoutConfirmed)
	state.Payout = &PayoutJournal{Prepared: validPayout(t, state, 901), TotalBefore: "2"}
	fx.store.current = &state
	fx.chain.events = nil
	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fx.chain.events) != 0 || len(fx.repo.completedIDs) == 0 {
		t.Fatalf("chain = %v, completed = %v", fx.chain.events, fx.repo.completedIDs)
	}
}

func TestRunRecoversCurrentWithoutStartingAnotherRun(t *testing.T) {
	fx := newFixture(t)
	manifest, err := BuildManifest(ManifestInput{
		Records:    []Record{{ID: 99, PeriodStartAt: 1, Amount: "0"}},
		Settlement: "0xsettlement", Recipient: "0xrecipient",
	})
	if err != nil {
		t.Fatal(err)
	}
	fx.store.current = &RunState{
		RunID: "existing", Trigger: TriggerUTC, UTCDate: "2026-07-11",
		Phase: PhasePrepared, Manifest: manifest,
	}

	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	if fx.repo.listCalls != 0 {
		t.Fatalf("ListPending() calls = %d, want 0", fx.repo.listCalls)
	}
	if !equalUint64(fx.repo.completedIDs, []uint64{99}) {
		t.Fatalf("completed IDs = %v, want recovered record only", fx.repo.completedIDs)
	}
	if len(fx.chain.events) != 0 {
		t.Fatalf("chain events = %v, want no second run", fx.chain.events)
	}
}

func TestRunRecoveryReportUsesFinalPhase(t *testing.T) {
	fx := newFixture(t)
	state := fx.positiveState(t, PhasePayoutSubmitting)
	state.Payout = &PayoutJournal{Prepared: validPayout(t, state, 904), TotalBefore: "2"}
	fx.store.current = &state
	fx.chain.balances["0xsettlement"] = decimal.RequireFromString("0.5")

	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	if len(fx.notifier.reports) != 1 || !strings.Contains(fx.notifier.reports[0], "phase: payout_confirmed") {
		t.Fatalf("reports = %#v", fx.notifier.reports)
	}
}

func TestNewOrchestratorRejectsUnsafeRecipient(t *testing.T) {
	fx := newFixture(t)
	for _, recipient := range []string{"0xbuilder", zeroEthereumAddress} {
		t.Run(recipient, func(t *testing.T) {
			_, err := NewOrchestrator(OrchestratorConfig{
				Repository: fx.repo, Store: fx.store, Chain: fx.chain,
				Builders: []string{"0xbuilder"}, Settlement: "0xsettlement", Recipient: recipient,
				Clock: fx.clock, Nonce: &fakeNonce{next: 1}, Sleeper: fx.sleeper,
			})
			if err == nil || !strings.Contains(err.Error(), "recipient") {
				t.Fatalf("NewOrchestrator() error = %v", err)
			}
		})
	}
}

func TestPayoutIsNotSubmittedUntilBothDurableBoundariesSucceed(t *testing.T) {
	fx := newFixture(t)
	fx.store.failPhase = PhasePayoutSubmitting
	err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart)
	if err == nil {
		t.Fatal("Run() error = nil")
	}
	if containsString(fx.chain.events, "submit:spotSend:0xsettlement") {
		t.Fatalf("payout submitted after state save failure: %v", fx.chain.events)
	}
	if fx.store.current == nil || fx.store.current.Phase != PhasePayoutSubmitting || fx.store.current.Payout == nil {
		t.Fatalf("durable current = %#v", fx.store.current)
	}
}

func TestRunNoDataArchivesWithoutChainMutation(t *testing.T) {
	fx := newFixture(t)
	fx.repo.records = nil
	if err := fx.orchestrator.Run(context.Background(), TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	if len(fx.chain.events) != 0 || fx.store.archiveResult != "" {
		t.Fatalf("chain = %v, archive = %q", fx.chain.events, fx.store.archiveResult)
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
		claimableErrors: map[string]error{},
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
		Repository: repo, Store: store, Chain: chain, Notifier: notifier, Logger: logger,
		Builders:   []string{"0xbuilder"},
		Settlement: "0xsettlement", Recipient: "0xrecipient",
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
	return RunState{RunID: "run", UTCDate: "2026-07-12", Phase: phase, Manifest: manifest}
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
	current       *RunState
	saved         []RunState
	archived      RunState
	archiveResult string
	failPhase     Phase
	metadata      StateLoadMetadata
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
func (s *fakeStore) Clear(context.Context) error { s.current = nil; return nil }

type fakeChain struct {
	token                   info.Token
	claimableRewards        map[string]decimal.Decimal
	claimableErrors         map[string]error
	claimableCalls          map[string]int
	balances                map[string]decimal.Decimal
	holds                   map[string]decimal.Decimal
	balanceErrors           map[string]error
	balanceSequence         map[string][]info.SpotBalanceAmounts
	balanceCalls            map[string]int
	rateLimits              map[string]info.UserRateLimit
	rateLimitErrors         map[string]error
	rateLimitCalls          map[string]int
	trackRateLimitUsage     bool
	respectRateLimitContext bool
	claimResult             exchange.SubmitResult
	sweepResult             exchange.SubmitResult
	payoutResult            exchange.SubmitResult
	applyUnknownPayout      bool
	events                  []string
}

func (c *fakeChain) CanonicalUSDC(context.Context) (info.Token, error) { return c.token, nil }
func (c *fakeChain) ClaimableUSDC(_ context.Context, address string, _ info.Token) (decimal.Decimal, error) {
	if c.claimableCalls == nil {
		c.claimableCalls = make(map[string]int)
	}
	c.claimableCalls[address]++
	if err := c.claimableErrors[address]; err != nil {
		return decimal.Zero, err
	}
	return c.claimableRewards[address], nil
}
func (c *fakeChain) SpotBalance(_ context.Context, address string, _ info.Token) (info.SpotBalanceAmounts, error) {
	c.balanceCalls[address]++
	if err := c.balanceErrors[address]; err != nil {
		return info.SpotBalanceAmounts{}, err
	}
	if sequence := c.balanceSequence[address]; len(sequence) > 0 {
		balance := sequence[0]
		c.balanceSequence[address] = sequence[1:]
		return balance, nil
	}
	return info.SpotBalanceAmounts{Total: c.balances[address], Available: c.balances[address].Sub(c.holds[address])}, nil
}
func (c *fakeChain) UserRateLimit(ctx context.Context, address string) (info.UserRateLimit, error) {
	c.rateLimitCalls[address]++
	if c.respectRateLimitContext && ctx.Err() != nil {
		return info.UserRateLimit{}, ctx.Err()
	}
	if err := c.rateLimitErrors[address]; err != nil {
		return info.UserRateLimit{}, err
	}
	return c.rateLimits[address], nil
}
func (c *fakeChain) PrepareClaim(address string, nonce uint64) (exchange.PreparedAction, error) {
	return preparedAction(tinyTB{}, "claimRewards", address, "", "", "", nonce), nil
}
func (c *fakeChain) PrepareSpotSend(address, destination string, token info.Token, amount decimal.Decimal, nonce uint64) (exchange.PreparedAction, error) {
	return preparedAction(tinyTB{}, "spotSend", address, destination, token.WireToken, amount.String(), nonce), nil
}
func (c *fakeChain) Submit(_ context.Context, action exchange.PreparedAction) (exchange.SubmitResult, error) {
	c.events = append(c.events, "submit:"+action.Kind+":"+action.Signer)
	if c.trackRateLimitUsage {
		limit := c.rateLimits[action.Signer]
		limit.NRequestsUsed++
		c.rateLimits[action.Signer] = limit
	}
	if action.Kind == "claimRewards" {
		return c.claimResult, nil
	}
	amount := decimal.RequireFromString(action.Amount)
	if action.Signer == "0xsettlement" {
		if c.payoutResult.Accepted || c.applyUnknownPayout {
			c.balances[action.Signer] = c.balances[action.Signer].Sub(amount)
		}
		return c.payoutResult, nil
	}
	if c.sweepResult.Accepted {
		c.balances[action.Signer] = c.balances[action.Signer].Sub(amount)
		c.balances[action.Destination] = c.balances[action.Destination].Add(amount)
	}
	return c.sweepResult, nil
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
	alertMessages []string
	reports       []string
}

func (n *fakeNotifier) Alert(_ context.Context, key, message string) {
	n.alerts = append(n.alerts, key)
	n.alertMessages = append(n.alertMessages, message)
}
func (n *fakeNotifier) Report(_ context.Context, _ string, message string) {
	n.reports = append(n.reports, message)
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

func assertRateLimitLog(t *testing.T, logger *fakeLogger, address, level string, want map[string]any) {
	t.Helper()
	for _, entry := range logger.entries {
		if entry.attrs["event"] != "funding_user_rate_limit_observed" || entry.attrs["address"] != address {
			continue
		}
		if entry.level != level {
			t.Fatalf("rate limit log for %s level = %q, want %q", address, entry.level, level)
		}
		for key, value := range want {
			if got := entry.attrs[key]; got != value {
				t.Fatalf("rate limit log for %s field %s = %#v, want %#v", address, key, got, value)
			}
		}
		for _, key := range []string{"requests_cap", "requests_used", "alert_threshold", "below_threshold"} {
			if _, exists := entry.attrs[key]; exists {
				t.Fatalf("rate limit log for %s unexpectedly contains field %s", address, key)
			}
		}
		return
	}
	t.Fatalf("rate limit observation for %s was not logged", address)
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type fakeNonce struct{ next uint64 }

func (n *fakeNonce) Next() uint64 { n.next++; return n.next }

// tinyTB lets fake preparation share the same deterministic request builder.
type tinyTB struct{}

func (tinyTB) Helper()           {}
func (tinyTB) Fatal(args ...any) { panic(fmt.Sprint(args...)) }

type testTB interface {
	Helper()
	Fatal(...any)
}

func preparedAction(t testTB, kind, signer, destination, token, amount string, nonce uint64) exchange.PreparedAction {
	t.Helper()
	body := map[string]any{"action": map[string]any{"type": kind}, "nonce": nonce}
	if kind == "spotSend" {
		body["action"] = map[string]any{"type": kind, "destination": destination, "token": token, "amount": amount, "time": nonce}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	return exchange.PreparedAction{Kind: kind, Signer: signer, Destination: destination, Token: token, Amount: amount,
		Nonce: nonce, RequestBody: raw, RequestHash: hex.EncodeToString(digest[:])}
}

func validPayout(t *testing.T, state RunState, nonce uint64) exchange.PreparedAction {
	return preparedAction(t, "spotSend", state.Manifest.Settlement, state.Manifest.Recipient,
		state.Manifest.Token.WireToken, state.Manifest.PayoutTotal, nonce)
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

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
func equalUint64(left, right []uint64) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
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
