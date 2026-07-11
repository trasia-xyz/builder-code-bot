package funding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"hyperliquid-builder-code-bot/internal/hyperliquid/exchange"
	"hyperliquid-builder-code-bot/internal/hyperliquid/info"
	"hyperliquid-builder-code-bot/internal/logging"
)

func TestRecoverRetriesPersistedBuilderBalanceQueryFailureWithoutMissingRequest(t *testing.T) {
	fx := newOrchestratorFixture(t)
	state := positiveState(t, PhaseConsolidating)
	state.Builders = []BuilderProgress{{
		Name: "one", Address: "0xbuilder1",
		Claim: ActionProgress{Phase: ActionAccepted},
		Sweep: ActionProgress{Phase: ActionUnknown, Response: json.RawMessage(`{"error":"operation failed"}`)},
	}}
	fx.store.current = &state
	fx.chain.balances = map[string]decimal.Decimal{
		"0xbuilder1":   decimal.NewFromInt(1),
		"0xsettlement": decimal.NewFromInt(2),
	}
	fx.chain.submitResults = []submitOutcome{
		{result: exchange.SubmitResult{Accepted: true}},
		{result: exchange.SubmitResult{Accepted: true}},
	}

	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fx.repo.completeCalls != 1 {
		t.Fatalf("complete calls = %d, want 1", fx.repo.completeCalls)
	}
	for _, want := range []string{
		"balance:0xbuilder1",
		"prepare_send:0xbuilder1:0xsettlement:1",
		"submit:spotSend:0xbuilder1",
	} {
		if !slicesContain(fx.chain.events, want) {
			t.Fatalf("events = %v, missing %q", fx.chain.events, want)
		}
	}
}

func TestRecoverBuilderReconciliationErrorsDoNotReplayAndAllowFundedPayout(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*orchestratorFixture)
	}{
		{name: "ledger query", setup: func(fx *orchestratorFixture) {
			fx.chain.transferOutcomes = []transferOutcome{{err: assertSecretError("ledger-secret")}}
		}},
		{name: "balance query", setup: func(fx *orchestratorFixture) {
			fx.chain.balanceErr = map[string]error{"0xbuilder1": assertSecretError("balance-secret")}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := logging.New(logging.Config{Format: logging.FormatJSON, Level: logging.LevelDebug, Output: &output})
			fx := newOrchestratorFixtureWithLogger(t, logger)
			state := positiveState(t, PhaseConsolidating)
			action := prepared("spotSend", "0xbuilder1", "0xsettlement", "1", 77)
			action.Token = "USDC:0"
			state.Builders = []BuilderProgress{{
				Name: "one", Address: "0xbuilder1", Claim: ActionProgress{Phase: ActionAccepted},
				Sweep: ActionProgress{Phase: ActionUnknown, Prepared: &action, SubmitAttempts: 1, BalanceBefore: "1"},
			}}
			fx.store.current = &state
			fx.chain.balances = map[string]decimal.Decimal{"0xbuilder1": decimal.NewFromInt(1), "0xsettlement": decimal.NewFromInt(2)}
			tt.setup(fx)

			if err := fx.orchestrator.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			if fx.repo.completeCalls != 1 || countPrefix(fx.chain.events, "submit:spotSend:0xbuilder1") != 0 || countPrefix(fx.chain.events, "submit:spotSend:0xsettlement") != 1 {
				t.Fatalf("unsafe continuation: events = %v, completes = %d", fx.chain.events, fx.repo.completeCalls)
			}
			foundUnknown := false
			for _, snapshot := range fx.store.saved {
				if len(snapshot.Builders) == 1 && snapshot.Builders[0].Sweep.Phase == ActionUnknown {
					foundUnknown = true
				}
			}
			if !foundUnknown {
				t.Fatal("builder unknown state was not persisted")
			}
			if countAlertSuffix(fx.notifier.keys, "chain_result_unconfirmed") != 1 || countAlertSuffix(fx.notifier.keys, "recovery_failed") != 0 {
				t.Fatalf("alert keys = %v", fx.notifier.keys)
			}
			for _, secret := range []string{"ledger-secret", "balance-secret"} {
				if bytes.Contains(output.Bytes(), []byte(secret)) {
					t.Fatalf("logs leaked %q: %s", secret, output.String())
				}
			}
		})
	}
}

func TestRecoverPayoutReconciliationErrorsPersistBlockedAndDoNotReplay(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*orchestratorFixture, *ActionProgress)
	}{
		{name: "ledger query", setup: func(fx *orchestratorFixture, _ *ActionProgress) {
			fx.chain.transferOutcomes = []transferOutcome{{err: assertSecretError("ledger-secret")}, {err: assertSecretError("ledger-secret")}}
		}},
		{name: "balance query", setup: func(fx *orchestratorFixture, _ *ActionProgress) {
			fx.chain.balanceErr = map[string]error{"0xsettlement": assertSecretError("balance-secret")}
		}},
		{name: "balance parse", setup: func(_ *orchestratorFixture, progress *ActionProgress) {
			progress.BalanceBefore = "not-a-decimal"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newOrchestratorFixture(t)
			state := positiveState(t, PhasePayoutSubmitting)
			action := prepared("spotSend", "0xsettlement", "0xrecipient", "1.25", 77)
			action.Token = "USDC:0"
			state.FinalPayout = &ActionProgress{Phase: ActionUnknown, Prepared: &action, SubmitAttempts: 1, BalanceBefore: "5"}
			fx.store.current = &state
			fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.NewFromInt(5)}
			tt.setup(fx, state.FinalPayout)

			if err := fx.orchestrator.Recover(context.Background()); err == nil {
				t.Fatal("Recover() error = nil")
			}
			if err := fx.orchestrator.Recover(context.Background()); err == nil {
				t.Fatal("second Recover() error = nil")
			}
			if fx.store.current == nil || fx.store.current.Phase != PhaseBlocked || fx.store.current.FinalPayout.Phase != ActionUnknown {
				t.Fatalf("current = %#v", fx.store.current)
			}
			if countPrefix(fx.chain.events, "submit:") != 0 || fx.repo.completeCalls != 0 {
				t.Fatalf("unsafe replay: events = %v, completes = %d", fx.chain.events, fx.repo.completeCalls)
			}
			if countAlertSuffix(fx.notifier.keys, "chain_result_unconfirmed") != 1 || countAlertSuffix(fx.notifier.keys, "recovery_failed") != 1 {
				t.Fatalf("alert keys = %v", fx.notifier.keys)
			}
		})
	}
}

func TestRecoverObservesBackupFallbackAndAlertsOnce(t *testing.T) {
	var output bytes.Buffer
	logger := logging.New(logging.Config{Format: logging.FormatJSON, Level: logging.LevelDebug, Output: &output})
	fx := newOrchestratorFixtureWithLogger(t, logger)
	state := positiveState(t, PhaseCompleted)
	state.DBCompleted = true
	fx.store.current = &state
	fx.store.loadMeta = StateLoadMetadata{RecoveredFromBackup: true, PrimaryInvalid: true}

	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countAlertSuffix(fx.notifier.keys, "state_corruption") != 1 {
		t.Fatalf("alert keys = %v", fx.notifier.keys)
	}
	if !bytes.Contains(output.Bytes(), []byte(`"event":"state_snapshot_recovered_from_backup"`)) {
		t.Fatalf("missing backup warning: %s", output.String())
	}
}

func TestRecoverBackupPreparedPayoutTreatsRequestAsAmbiguous(t *testing.T) {
	fx := newOrchestratorFixture(t)
	state := positiveState(t, PhaseFunded)
	action := prepared("spotSend", "0xsettlement", "0xrecipient", "1.25", 77)
	action.Token = "USDC:0"
	state.FinalPayout = &ActionProgress{Phase: ActionPrepared, Prepared: &action, BalanceBefore: "5"}
	fx.store.current = &state
	fx.store.loadMeta = StateLoadMetadata{RecoveredFromBackup: true, PrimaryInvalid: true}
	fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.RequireFromString("3.75")}
	fx.chain.transferMatched = true
	fx.chain.transfer = &info.LedgerUpdate{Time: 77, Delta: info.SpotTransferDelta{
		Type: "spotTransfer", Token: "USDC", Amount: "1.25",
		User: "0xsettlement", Destination: "0xrecipient",
	}}

	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countPrefix(fx.chain.events, "find:0xsettlement:0xrecipient:77") != 1 || countPrefix(fx.chain.events, "submit:") != 0 || fx.repo.completeCalls != 1 {
		t.Fatalf("events = %v, complete calls = %d", fx.chain.events, fx.repo.completeCalls)
	}
}

func TestRecoverBackupPreparedBuilderSweepReconcilesBeforeReplay(t *testing.T) {
	fx := newOrchestratorFixture(t)
	state := positiveState(t, PhaseConsolidating)
	action := prepared("spotSend", "0xbuilder1", "0xsettlement", "1", 77)
	action.Token = "USDC:0"
	state.Builders = []BuilderProgress{{
		Name: "one", Address: "0xbuilder1", Claim: ActionProgress{Phase: ActionAccepted},
		Sweep: ActionProgress{Phase: ActionPrepared, Prepared: &action, BalanceBefore: "1"},
	}}
	fx.store.current = &state
	fx.store.loadMeta = StateLoadMetadata{RecoveredFromBackup: true, PrimaryInvalid: true}
	fx.chain.balances = map[string]decimal.Decimal{"0xbuilder1": decimal.Zero, "0xsettlement": decimal.NewFromInt(2)}
	fx.chain.transferMatched = true
	fx.chain.transfer = &info.LedgerUpdate{Time: 77, Delta: info.SpotTransferDelta{
		Type: "spotTransfer", Token: "USDC", Amount: "1",
		User: "0xbuilder1", Destination: "0xsettlement",
	}}

	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countPrefix(fx.chain.events, "find:0xbuilder1:0xsettlement:77") != 1 || countPrefix(fx.chain.events, "submit:spotSend:0xbuilder1") != 0 || fx.repo.completeCalls != 1 {
		t.Fatalf("events = %v, complete calls = %d", fx.chain.events, fx.repo.completeCalls)
	}
}

func TestRecoverBackupPreparedPayoutHasOnlyOneReplayAfterNoEvidence(t *testing.T) {
	tests := []struct {
		name    string
		outcome submitOutcome
	}{
		{name: "ambiguous", outcome: submitOutcome{}},
		{name: "rejected", outcome: submitOutcome{result: exchange.SubmitResult{Rejected: true}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newOrchestratorFixture(t)
			state := positiveState(t, PhaseFunded)
			action := prepared("spotSend", "0xsettlement", "0xrecipient", "1.25", 77)
			action.Token = "USDC:0"
			state.FinalPayout = &ActionProgress{Phase: ActionPrepared, Prepared: &action, BalanceBefore: "5"}
			fx.store.current = &state
			fx.store.loadMeta = StateLoadMetadata{RecoveredFromBackup: true, PrimaryInvalid: true}
			fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.NewFromInt(5)}
			fx.chain.submitResults = []submitOutcome{tt.outcome}

			for attempt := 0; attempt < 2; attempt++ {
				if err := fx.orchestrator.Recover(context.Background()); err == nil {
					t.Fatalf("Recover() attempt %d error = nil", attempt+1)
				}
			}
			if countPrefix(fx.chain.events, "find:0xsettlement:0xrecipient:77") < 1 {
				t.Fatalf("events = %v, want query-first recovery", fx.chain.events)
			}
			if got := countPrefix(fx.chain.events, "submit:spotSend:0xsettlement"); got != 1 {
				t.Fatalf("payout submits = %d, want 1; events = %v", got, fx.chain.events)
			}
			if countPrefix(fx.chain.events, "prepare_send:") != 0 {
				t.Fatalf("recovery prepared a new payout: %v", fx.chain.events)
			}
			if fx.store.current == nil || fx.store.current.FinalPayout == nil || fx.store.current.FinalPayout.SubmitAttempts != 2 || fx.store.current.FinalPayout.Phase != ActionUnknown {
				t.Fatalf("current = %#v", fx.store.current)
			}
		})
	}
}

func TestRecoverBackupPreparedSweepHasOnlyOneReplayAfterNoEvidence(t *testing.T) {
	tests := []struct {
		name    string
		outcome submitOutcome
	}{
		{name: "ambiguous", outcome: submitOutcome{}},
		{name: "rejected", outcome: submitOutcome{result: exchange.SubmitResult{Rejected: true}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newOrchestratorFixture(t)
			state := positiveState(t, PhaseConsolidating)
			action := prepared("spotSend", "0xbuilder1", "0xsettlement", "1", 77)
			action.Token = "USDC:0"
			state.Builders = []BuilderProgress{{
				Name: "one", Address: "0xbuilder1", Claim: ActionProgress{Phase: ActionAccepted},
				Sweep: ActionProgress{Phase: ActionPrepared, Prepared: &action, BalanceBefore: "1"},
			}}
			fx.store.current = &state
			fx.store.loadMeta = StateLoadMetadata{RecoveredFromBackup: true, PrimaryInvalid: true}
			fx.chain.balances = map[string]decimal.Decimal{
				"0xbuilder1": decimal.NewFromInt(1), "0xsettlement": decimal.Zero,
			}
			fx.chain.submitResults = []submitOutcome{tt.outcome}

			for attempt := 0; attempt < 2; attempt++ {
				if err := fx.orchestrator.Recover(context.Background()); err == nil {
					t.Fatalf("Recover() attempt %d error = nil", attempt+1)
				}
			}
			if countPrefix(fx.chain.events, "find:0xbuilder1:0xsettlement:77") < 1 {
				t.Fatalf("events = %v, want query-first recovery", fx.chain.events)
			}
			if got := countPrefix(fx.chain.events, "submit:spotSend:0xbuilder1"); got != 1 {
				t.Fatalf("builder sweep submits = %d, want 1; events = %v", got, fx.chain.events)
			}
			if countPrefix(fx.chain.events, "prepare_send:0xbuilder1") != 0 {
				t.Fatalf("recovery prepared a new sweep: %v", fx.chain.events)
			}
			if fx.store.current == nil || len(fx.store.current.Builders) != 1 || fx.store.current.Builders[0].Sweep.SubmitAttempts != 2 || fx.store.current.Builders[0].Sweep.Phase != ActionUnknown {
				t.Fatalf("current = %#v", fx.store.current)
			}
		})
	}
}

func TestRecoverBackupPreparedClaimIsUnconfirmableAndDoesNotBlockSweep(t *testing.T) {
	var output bytes.Buffer
	logger := logging.New(logging.Config{Format: logging.FormatJSON, Level: logging.LevelDebug, Output: &output})
	fx := newOrchestratorFixtureWithLogger(t, logger)
	state := positiveState(t, PhaseConsolidating)
	claim := prepared("claimRewards", "0xbuilder1", "", "", 77)
	state.Builders = []BuilderProgress{{
		Name: "one", Address: "0xbuilder1",
		Claim: ActionProgress{Phase: ActionPrepared, Prepared: &claim, Response: json.RawMessage(`{"error":"private_key=claim-secret"}`)},
	}}
	fx.store.current = &state
	fx.store.loadMeta = StateLoadMetadata{RecoveredFromBackup: true, PrimaryInvalid: true}
	fx.chain.balances = map[string]decimal.Decimal{
		"0xbuilder1": decimal.NewFromInt(1), "0xsettlement": decimal.Zero,
	}
	fx.chain.submitResults = []submitOutcome{{result: exchange.SubmitResult{Accepted: true}}}

	if err := fx.orchestrator.Recover(context.Background()); err == nil {
		t.Fatal("Recover() error = nil, want underfunded settlement")
	}
	// Simulate a restart that loads the newly persisted primary snapshot rather
	// than falling back to the stale backup again.
	fx.store.loadMeta = StateLoadMetadata{}
	if err := fx.orchestrator.Recover(context.Background()); err == nil {
		t.Fatal("second Recover() error = nil, want underfunded settlement")
	}

	if countPrefix(fx.chain.events, "prepare_claim:") != 0 || countPrefix(fx.chain.events, "submit:claimRewards:") != 0 {
		t.Fatalf("stale claim was replayed: %v", fx.chain.events)
	}
	if countPrefix(fx.chain.events, "balance:0xbuilder1") < 1 || countPrefix(fx.chain.events, "prepare_send:0xbuilder1:0xsettlement:1") != 1 || countPrefix(fx.chain.events, "submit:spotSend:0xbuilder1") != 1 {
		t.Fatalf("builder sweep did not proceed exactly once: %v", fx.chain.events)
	}
	if fx.store.current == nil || len(fx.store.current.Builders) != 1 || fx.store.current.Builders[0].Claim.Phase != ActionUnknown || fx.store.current.Builders[0].Claim.SubmitAttempts < 1 || !fx.store.current.Builders[0].Claim.Unconfirmable {
		t.Fatalf("current = %#v", fx.store.current)
	}
	if countAlertSuffix(fx.notifier.keys, "backup_claim_unconfirmable") != 1 {
		t.Fatalf("alert keys = %v", fx.notifier.keys)
	}
	logs := output.String()
	if !strings.Contains(logs, `"event":"backup_claim_unconfirmable"`) || strings.Contains(logs, "claim-secret") {
		t.Fatalf("logs = %s", logs)
	}
}

func TestRecoverPrimaryPreparedClaimStillReplaysExactRequest(t *testing.T) {
	fx := newOrchestratorFixture(t)
	state := positiveState(t, PhaseConsolidating)
	claim := prepared("claimRewards", "0xbuilder1", "", "", 77)
	state.Builders = []BuilderProgress{{
		Name: "one", Address: "0xbuilder1",
		Claim: ActionProgress{Phase: ActionPrepared, Prepared: &claim},
		Sweep: ActionProgress{Phase: ActionZeroBalance},
	}}
	fx.store.current = &state
	fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.NewFromInt(2)}
	fx.chain.submitResults = []submitOutcome{
		{result: exchange.SubmitResult{Accepted: true}},
		{result: exchange.SubmitResult{Accepted: true}},
	}

	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countPrefix(fx.chain.events, "prepare_claim:") != 0 || countPrefix(fx.chain.events, "submit:claimRewards:0xbuilder1") != 1 {
		t.Fatalf("primary prepared claim did not exact replay: %v", fx.chain.events)
	}
}

func TestRecoverAfterAcceptedPayoutSaveFailureUsesEvidenceWithoutReplay(t *testing.T) {
	fx := newOrchestratorFixture(t)
	fx.repo.records = []Record{{ID: 83, Amount: "1"}}
	fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.NewFromInt(2)}
	fx.store.failSave = func(state RunState) bool { return state.Phase == PhasePayoutAccepted }

	if err := fx.orchestrator.RunNew(context.Background(), TriggerUTC); err == nil {
		t.Fatal("RunNew() error = nil")
	}
	if fx.store.current == nil || fx.store.current.Phase != PhasePayoutSubmitting || fx.store.current.FinalPayout == nil || fx.store.current.FinalPayout.Prepared == nil {
		t.Fatalf("current = %#v", fx.store.current)
	}
	if countPrefix(fx.chain.events, "submit:spotSend:0xsettlement") != 1 {
		t.Fatalf("events before recovery = %v", fx.chain.events)
	}
	prepared := fx.store.current.FinalPayout.Prepared
	fx.chain.transferMatched = true
	fx.chain.transfer = &info.LedgerUpdate{Time: prepared.Nonce, Delta: info.SpotTransferDelta{
		Type: "spotTransfer", Token: "USDC", Amount: info.DecimalText(prepared.Amount),
		User: prepared.Signer, Destination: prepared.Destination,
	}}
	before := decimal.RequireFromString(fx.store.current.FinalPayout.BalanceBefore)
	amount := decimal.RequireFromString(prepared.Amount)
	fx.chain.balances["0xsettlement"] = before.Sub(amount)
	fx.store.failSave = nil

	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countPrefix(fx.chain.events, "submit:spotSend:0xsettlement") != 1 || fx.repo.completeCalls != 1 {
		t.Fatalf("events = %v, complete calls = %d", fx.chain.events, fx.repo.completeCalls)
	}
}

func TestRecoverLoadErrorIsSanitizedAndAlerted(t *testing.T) {
	var output bytes.Buffer
	logger := logging.New(logging.Config{Format: logging.FormatJSON, Level: logging.LevelDebug, Output: &output})
	fx := newOrchestratorFixtureWithLogger(t, logger)
	fx.store.loadErr = assertSecretError("private_key=load-secret")

	if err := fx.orchestrator.Recover(context.Background()); err == nil {
		t.Fatal("Recover() error = nil")
	}
	if countAlertSuffix(fx.notifier.keys, "state_load_failed") != 1 {
		t.Fatalf("alert keys = %v", fx.notifier.keys)
	}
	if bytes.Contains(output.Bytes(), []byte("load-secret")) || !bytes.Contains(output.Bytes(), []byte(`"event":"state_load_failed"`)) {
		t.Fatalf("logs = %s", output.String())
	}
}

func TestRunNewTerminalArchivesContainVerifiableManifest(t *testing.T) {
	tests := []struct {
		name    string
		records []Record
		result  string
	}{
		{name: "no data", result: "no_data"},
		{name: "failed validation", records: []Record{{ID: 9, Amount: "-0.01"}}, result: "failed_validation"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newOrchestratorFixture(t)
			fx.repo.records = tt.records
			err := fx.orchestrator.RunNew(context.Background(), TriggerUTC)
			if tt.result == "no_data" && err != nil {
				t.Fatal(err)
			}
			if tt.result == "failed_validation" && !errors.Is(err, ErrNegativeAmount) {
				t.Fatalf("RunNew() error = %v", err)
			}
			if len(fx.store.archived) != 1 || fx.store.archives[0] != tt.result {
				t.Fatalf("archives = %v, states = %#v", fx.store.archives, fx.store.archived)
			}
			archived := fx.store.archived[0]
			got, hashErr := HashManifest(archived.Manifest)
			if hashErr != nil || archived.Manifest.ManifestHash == "" || got != archived.Manifest.ManifestHash {
				t.Fatalf("archived manifest = %#v, hash = %q, err = %v", archived.Manifest, got, hashErr)
			}
		})
	}
}

func TestRecoverEventsBracketEverySuccessfulPersistedPath(t *testing.T) {
	tests := []struct {
		name  string
		state func(*testing.T) RunState
		setup func(*orchestratorFixture)
	}{
		{name: "prepared", state: func(t *testing.T) RunState {
			state := positiveState(t, PhasePrepared)
			state.Builders = nil
			return state
		}, setup: func(fx *orchestratorFixture) {
			fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.NewFromInt(2)}
		}},
		{name: "funded", state: func(t *testing.T) RunState { return positiveState(t, PhaseFunded) }, setup: func(fx *orchestratorFixture) {
			fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.NewFromInt(2)}
		}},
		{name: "payout accepted", state: func(t *testing.T) RunState {
			state := positiveState(t, PhasePayoutAccepted)
			action := prepared("spotSend", "0xsettlement", "0xrecipient", "1.25", 77)
			action.Token = "USDC:0"
			state.FinalPayout = &ActionProgress{Phase: ActionAccepted, Prepared: &action}
			return state
		}},
		{name: "db updating", state: func(t *testing.T) RunState { return positiveState(t, PhaseDBUpdating) }},
		{name: "completed", state: func(t *testing.T) RunState {
			state := positiveState(t, PhaseCompleted)
			state.DBCompleted = true
			return state
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := logging.New(logging.Config{Format: logging.FormatJSON, Level: logging.LevelDebug, Output: &output})
			fx := newOrchestratorFixtureWithLogger(t, logger)
			state := tt.state(t)
			fx.store.current = &state
			if tt.setup != nil {
				tt.setup(fx)
			}
			if err := fx.orchestrator.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			logs := output.String()
			if strings.Count(logs, `"event":"recovery_started"`) != 1 || strings.Count(logs, `"event":"recovery_completed"`) != 1 || strings.Contains(logs, `"event":"recovery_failed"`) {
				t.Fatalf("logs = %s", logs)
			}
		})
	}
}

func TestRecoverWithoutCurrentEmitsNoRecoveryEvents(t *testing.T) {
	var output bytes.Buffer
	logger := logging.New(logging.Config{Format: logging.FormatJSON, Level: logging.LevelDebug, Output: &output})
	fx := newOrchestratorFixtureWithLogger(t, logger)
	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "recovery_") {
		t.Fatalf("logs = %s", output.String())
	}
}

func TestRecoverFailureEmitsStartedAndFailedWithoutCompleted(t *testing.T) {
	var output bytes.Buffer
	logger := logging.New(logging.Config{Format: logging.FormatJSON, Level: logging.LevelDebug, Output: &output})
	fx := newOrchestratorFixtureWithLogger(t, logger)
	state := positiveState(t, PhasePayoutSubmitting)
	action := prepared("spotSend", "0xsettlement", "0xrecipient", "1.25", 77)
	action.Token = "USDC:0"
	state.FinalPayout = &ActionProgress{Phase: ActionUnknown, Prepared: &action, SubmitAttempts: 1, BalanceBefore: "5"}
	fx.store.current = &state
	fx.chain.transferOutcomes = []transferOutcome{{err: assertSecretError("private_key=ledger-secret")}}

	if err := fx.orchestrator.Recover(context.Background()); err == nil {
		t.Fatal("Recover() error = nil")
	}
	logs := output.String()
	if strings.Count(logs, `"event":"recovery_started"`) != 1 || strings.Count(logs, `"event":"recovery_failed"`) != 1 || strings.Contains(logs, `"event":"recovery_completed"`) {
		t.Fatalf("logs = %s", logs)
	}
	if strings.Contains(logs, "ledger-secret") {
		t.Fatalf("logs leaked raw error: %s", logs)
	}
}

type secretError string

func (e secretError) Error() string { return string(e) }

func assertSecretError(value string) error { return secretError(value) }

func TestRecoverBuilderSweepReconcilesAgainAfterSameNonceReplayRejected(t *testing.T) {
	fx := newOrchestratorFixture(t)
	state := positiveState(t, PhaseConsolidating)
	action := prepared("spotSend", "0xbuilder1", "0xsettlement", "1", 77)
	action.Token = "USDC:0"
	state.Builders = []BuilderProgress{{
		Name: "one", Address: "0xbuilder1",
		Claim: ActionProgress{Phase: ActionAccepted},
		Sweep: ActionProgress{
			Phase: ActionUnknown, Prepared: &action,
			SubmitAttempts: 1, BalanceBefore: "1",
		},
	}}
	fx.store.current = &state
	fx.chain.balanceQueues = map[string][]decimal.Decimal{
		"0xbuilder1": {decimal.NewFromInt(1), decimal.Zero},
	}
	fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.NewFromInt(2)}
	evidence := &info.LedgerUpdate{Time: 77, Delta: info.SpotTransferDelta{
		Type: "spotTransfer", Token: "USDC", Amount: "1",
		User: "0xbuilder1", Destination: "0xsettlement",
	}}
	fx.chain.transferOutcomes = []transferOutcome{{}, {transfer: evidence, matched: true}}
	fx.chain.submitResults = []submitOutcome{
		{result: exchange.SubmitResult{Rejected: true}},
		{result: exchange.SubmitResult{Accepted: true}},
	}

	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countPrefix(fx.chain.events, "find:0xbuilder1:0xsettlement:77") != 2 {
		t.Fatalf("events = %v, want two reconciliation queries", fx.chain.events)
	}
	acceptedWithEvidence := false
	for _, snapshot := range fx.store.saved {
		if len(snapshot.Builders) == 1 && snapshot.Builders[0].Sweep.Phase == ActionAccepted && snapshot.Builders[0].Sweep.Evidence != nil {
			acceptedWithEvidence = true
		}
	}
	if !acceptedWithEvidence {
		t.Fatalf("builder sweep was never persisted accepted with evidence; snapshots = %#v", fx.store.saved)
	}
}

func TestRecoverAmbiguousClaimReplayRejectedRemainsUnknown(t *testing.T) {
	fx := newOrchestratorFixture(t)
	state := positiveState(t, PhaseConsolidating)
	claim := prepared("claimRewards", "0xbuilder1", "", "", 77)
	state.Builders = []BuilderProgress{{
		Name: "one", Address: "0xbuilder1",
		Claim: ActionProgress{Phase: ActionUnknown, Prepared: &claim, SubmitAttempts: 1},
		Sweep: ActionProgress{Phase: ActionZeroBalance},
	}}
	fx.store.current = &state
	fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.NewFromInt(2)}
	fx.chain.submitResults = []submitOutcome{
		{result: exchange.SubmitResult{Rejected: true}},
		{result: exchange.SubmitResult{Accepted: true}},
	}

	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	foundUnknown := false
	for _, snapshot := range fx.store.saved {
		if len(snapshot.Builders) == 1 && snapshot.Builders[0].Claim.Phase == ActionUnknown {
			foundUnknown = true
		}
		if len(snapshot.Builders) == 1 && snapshot.Builders[0].Claim.Phase == ActionRejected {
			t.Fatalf("ambiguous claim was persisted as definitively rejected: %#v", snapshot.Builders[0].Claim)
		}
	}
	if !foundUnknown || fx.repo.completeCalls != 1 {
		t.Fatalf("unknown persisted = %v, complete calls = %d", foundUnknown, fx.repo.completeCalls)
	}
}

func TestRecoveryCrashMatrixResumesFromDurableCurrent(t *testing.T) {
	tests := []struct {
		name     string
		amount   string
		boundary func(RunState) bool
	}{
		{name: "prepared", amount: "1", boundary: func(state RunState) bool { return state.Phase == PhasePrepared }},
		{name: "funded", amount: "1", boundary: func(state RunState) bool { return state.Phase == PhaseFunded }},
		{name: "db updating", amount: "0", boundary: func(state RunState) bool { return state.Phase == PhaseDBUpdating }},
		{name: "completed", amount: "0", boundary: func(state RunState) bool { return state.Phase == PhaseCompleted }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newOrchestratorFixture(t)
			fx.repo.records = []Record{{ID: 81, Amount: tt.amount}}
			fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.NewFromInt(2)}
			crashed := false
			fx.store.crashAfterSave = func(state RunState) bool {
				if !crashed && tt.boundary(state) {
					crashed = true
					return true
				}
				return false
			}
			if err := fx.orchestrator.RunNew(context.Background(), TriggerUTC); err == nil || !crashed {
				t.Fatalf("RunNew() error = %v, crashed = %v", err, crashed)
			}
			if fx.store.current == nil {
				t.Fatal("durable current missing after simulated exit")
			}
			fx.store.crashAfterSave = nil
			if err := fx.orchestrator.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			if fx.store.current != nil || fx.repo.completeCalls != 1 {
				t.Fatalf("current = %#v, complete calls = %d", fx.store.current, fx.repo.completeCalls)
			}
			if tt.amount != "0" && countPrefix(fx.chain.events, "submit:spotSend:0xsettlement") != 1 {
				t.Fatalf("events = %v", fx.chain.events)
			}
		})
	}
}

func TestRecoverBlockedRunContinuesAfterSettlementIsFunded(t *testing.T) {
	fx := newOrchestratorFixture(t)
	fx.repo.records = []Record{{ID: 82, Amount: "1"}}
	fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.Zero}
	if err := fx.orchestrator.RunNew(context.Background(), TriggerUTC); err == nil {
		t.Fatal("RunNew() error = nil")
	}
	if fx.store.current == nil || fx.store.current.Phase != PhaseBlocked {
		t.Fatalf("current = %#v", fx.store.current)
	}
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fx.repo.completeCalls != 1 || fx.store.current != nil {
		t.Fatalf("complete calls = %d, current = %#v", fx.repo.completeCalls, fx.store.current)
	}
}

func TestRunWithNoCurrentAndNoPendingArchivesOnlyNoData(t *testing.T) {
	fx := newOrchestratorFixture(t)
	if err := fx.orchestrator.Run(context.Background(), TriggerUTC); err != nil {
		t.Fatal(err)
	}
	if fx.store.saveCalls != 0 || fx.chain.calls != 0 || fx.repo.completeCalls != 0 || len(fx.store.archives) != 1 || fx.store.archives[0] != "no_data" {
		t.Fatalf("saves = %d, chain = %d, complete = %d, archives = %v", fx.store.saveCalls, fx.chain.calls, fx.repo.completeCalls, fx.store.archives)
	}
	if len(fx.notifier.reports) != 1 {
		t.Fatalf("reports = %q, want exactly one", fx.notifier.reports)
	}
}
