package funding

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"hyperliquid-builder-code-bot/internal/logging"
)

func TestRunNewFailureAlertsAreDeduplicatedAndSanitized(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*orchestratorFixture)
		want  string
	}{
		{name: "negative amount", setup: func(fx *orchestratorFixture) {
			fx.repo.records = []Record{{ID: 1, Amount: "-0.000000000000000001"}}
		}, want: "validation_failed"},
		{name: "claim prepare", setup: func(fx *orchestratorFixture) {
			fx.repo.records = []Record{{ID: 1, Amount: "1"}}
			fx.chain.prepareClaimErr = map[string]error{"0xbuilder1": errors.New("private_key=claim-secret")}
			fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.NewFromInt(2)}
		}, want: "builder_claim_prepare_failed"},
		{name: "builder balance", setup: func(fx *orchestratorFixture) {
			fx.repo.records = []Record{{ID: 1, Amount: "1"}}
			fx.chain.balanceErr = map[string]error{"0xbuilder1": errors.New("password=balance-secret")}
			fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.NewFromInt(2)}
		}, want: "builder_balance_failed"},
		{name: "sweep prepare", setup: func(fx *orchestratorFixture) {
			fx.repo.records = []Record{{ID: 1, Amount: "1"}}
			fx.chain.balances = map[string]decimal.Decimal{"0xbuilder1": decimal.NewFromInt(1), "0xsettlement": decimal.NewFromInt(2)}
			fx.chain.prepareSendErr = map[string]error{"0xbuilder1": errors.New("mnemonic=sweep-secret")}
		}, want: "builder_sweep_prepare_failed"},
		{name: "database completion", setup: func(fx *orchestratorFixture) {
			fx.repo.records = []Record{{ID: 1, Amount: "0"}}
			fx.repo.completeErr = errors.New("dsn=db-secret")
		}, want: "database_completion_failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newOrchestratorFixture(t)
			tt.setup(fx)
			_ = fx.orchestrator.RunNew(context.Background(), TriggerUTC)
			if tt.want == "database_completion_failed" {
				_ = fx.orchestrator.Recover(context.Background())
			}
			if got := countAlertSuffix(fx.notifier.keys, tt.want); got != 1 {
				t.Fatalf("alerts %q = %d, keys = %v", tt.want, got, fx.notifier.keys)
			}
			joined := strings.Join(fx.notifier.messages, " ")
			for _, secret := range []string{"claim-secret", "balance-secret", "sweep-secret", "db-secret", "0xbuilder1"} {
				if strings.Contains(joined, secret) {
					t.Fatalf("alert leaked %q: %q", secret, joined)
				}
			}
		})
	}
}

func TestRunNewLogsStructuredRunPhaseActionAndDatabaseEventsWithoutSecrets(t *testing.T) {
	var output bytes.Buffer
	logger := logging.New(logging.Config{Format: logging.FormatJSON, Level: logging.LevelDebug, Output: &output})
	fx := newOrchestratorFixtureWithLogger(t, logger)
	fx.repo.records = []Record{{ID: 9, Amount: "1"}}
	fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.NewFromInt(2)}

	if err := fx.orchestrator.RunNew(context.Background(), TriggerUTC); err != nil {
		t.Fatal(err)
	}
	logs := output.String()
	for _, event := range []string{"funding_run_started", "funding_snapshot_loaded", "funding_phase_persisted", "funding_action_prepared", "funding_action_submitting", "funding_action_result", "funding_database_completed"} {
		if !strings.Contains(logs, `"event":"`+event+`"`) {
			t.Fatalf("missing event %q in logs:\n%s", event, logs)
		}
	}
	for _, secret := range []string{"request_body", "private_key", "0xbuilder1", "0xsettlement", "0xrecipient"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logs leaked %q:\n%s", secret, logs)
		}
	}
}

func TestRunNewReportsEveryOutcome(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(*orchestratorFixture)
		wantErr    bool
		wantReport []string
	}{
		{
			name: "completed",
			setup: func(fx *orchestratorFixture) {
				fx.repo.records = []Record{{ID: 9, Amount: "1"}}
				fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.NewFromInt(2)}
			},
			wantReport: []string{"Funding run succeeded", "status: succeeded", "outcome: completed", "record count: 1", "phase: completed", "payout total: 1"},
		},
		{
			name:       "no data",
			setup:      func(*orchestratorFixture) {},
			wantReport: []string{"Funding run succeeded", "status: succeeded", "outcome: no_data", "record count: 0"},
		},
		{
			name: "failed validation",
			setup: func(fx *orchestratorFixture) {
				fx.repo.records = []Record{{ID: 9, Amount: "-1"}}
			},
			wantErr:    true,
			wantReport: []string{"Funding run failed", "status: failed", "outcome: failed_validation", "record count: 1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newOrchestratorFixture(t)
			tt.setup(fx)
			err := fx.orchestrator.RunNew(context.Background(), TriggerUTC)
			if (err != nil) != tt.wantErr {
				t.Fatalf("RunNew() error = %v, wantErr = %v", err, tt.wantErr)
			}
			if len(fx.notifier.reports) != 1 {
				t.Fatalf("reports = %q, want exactly one", fx.notifier.reports)
			}
			for _, want := range tt.wantReport {
				if !strings.Contains(fx.notifier.reports[0], want) {
					t.Fatalf("report %q does not contain %q", fx.notifier.reports[0], want)
				}
			}
			for _, secret := range []string{"0xbuilder1", "0xsettlement", "0xrecipient"} {
				if strings.Contains(fx.notifier.reports[0], secret) {
					t.Fatalf("report leaked %q: %q", secret, fx.notifier.reports[0])
				}
			}
		})
	}
}

func TestRunNewSaveFailpointsCoverEveryMutationBoundary(t *testing.T) {
	boundaries := []struct {
		name   string
		match  func(RunState) bool
		submit int
	}{
		{name: "claim prepared", match: actionBoundary("claim", ActionPrepared), submit: 0},
		{name: "claim submitting", match: actionBoundary("claim", ActionSubmitting), submit: 0},
		{name: "claim accepted", match: actionBoundary("claim", ActionAccepted), submit: 1},
		{name: "sweep prepared", match: actionBoundary("sweep", ActionPrepared), submit: 2},
		{name: "sweep submitting", match: actionBoundary("sweep", ActionSubmitting), submit: 2},
		{name: "sweep accepted", match: actionBoundary("sweep", ActionAccepted), submit: 3},
		{name: "payout prepared", match: actionBoundary("payout", ActionPrepared), submit: 4},
		{name: "payout submitting", match: actionBoundary("payout", ActionSubmitting), submit: 4},
		{name: "payout accepted", match: func(s RunState) bool { return s.Phase == PhasePayoutAccepted }, submit: 5},
	}
	for _, tt := range boundaries {
		t.Run(tt.name, func(t *testing.T) {
			fx := newOrchestratorFixture(t)
			fx.repo.records = []Record{{ID: 1, Amount: "1"}}
			fx.chain.balances = map[string]decimal.Decimal{"0xbuilder1": decimal.NewFromInt(1), "0xbuilder2": decimal.NewFromInt(1), "0xsettlement": decimal.NewFromInt(2)}
			failed := false
			fx.store.failSave = func(state RunState) bool {
				if !failed && tt.match(state) {
					failed = true
					return true
				}
				return false
			}
			if err := fx.orchestrator.RunNew(context.Background(), TriggerUTC); err == nil {
				t.Fatal("RunNew() error = nil")
			}
			if !failed {
				t.Fatal("save failpoint was not reached")
			}
			if got := countPrefix(fx.chain.events, "submit:"); got != tt.submit {
				t.Fatalf("submit calls = %d, want %d; events = %v", got, tt.submit, fx.chain.events)
			}
		})
	}
}

func TestRunNewPersistsCompleteGlobalPhaseSequence(t *testing.T) {
	fx := newOrchestratorFixture(t)
	fx.repo.records = []Record{{ID: 1, Amount: "1"}}
	fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.NewFromInt(2)}
	if err := fx.orchestrator.RunNew(context.Background(), TriggerUTC); err != nil {
		t.Fatal(err)
	}
	var got []Phase
	for _, saved := range fx.store.saved {
		if len(got) == 0 || got[len(got)-1] != saved.Phase {
			got = append(got, saved.Phase)
		}
	}
	want := []Phase{PhasePrepared, PhaseConsolidating, PhaseFunded, PhasePayoutSubmitting, PhasePayoutAccepted, PhaseDBUpdating, PhaseCompleted}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("phases = %v, want %v", got, want)
	}
}

func TestRunNewRepositoryAndCompletionOrdering(t *testing.T) {
	fx := newOrchestratorFixture(t)
	fx.repo.records = []Record{{ID: 1, Amount: "0"}}
	if err := fx.orchestrator.RunNew(context.Background(), TriggerUTC); err != nil {
		t.Fatal(err)
	}
	want := []string{"repo.list_pending", "repo.complete", "store.save:completed:none", "store.archive:completed", "store.clear"}
	if got := filterEvents(*fx.events, want); !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered events = %v, want %v\nall events = %v", got, want, *fx.events)
	}
}

func TestCompleteDatabaseFailuresRetainAuthoritativeCurrentState(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*orchestratorFixture)
		phase Phase
	}{
		{name: "complete", setup: func(fx *orchestratorFixture) { fx.repo.completeErr = errors.New("db") }, phase: PhaseDBUpdating},
		{name: "completed save", setup: func(fx *orchestratorFixture) {
			fx.store.failSave = func(s RunState) bool { return s.Phase == PhaseCompleted }
		}, phase: PhaseDBUpdating},
		{name: "archive", setup: func(fx *orchestratorFixture) { fx.store.archiveErr = errors.New("archive") }, phase: PhaseCompleted},
		{name: "clear", setup: func(fx *orchestratorFixture) { fx.store.clearErr = errors.New("clear") }, phase: PhaseCompleted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newOrchestratorFixture(t)
			fx.repo.records = []Record{{ID: 1, Amount: "0"}}
			tt.setup(fx)
			if err := fx.orchestrator.RunNew(context.Background(), TriggerUTC); err == nil {
				t.Fatal("RunNew() error = nil")
			}
			if fx.store.current == nil || fx.store.current.Phase != tt.phase {
				t.Fatalf("current = %#v, want phase %s", fx.store.current, tt.phase)
			}
			if countAlertSuffix(fx.notifier.keys, "database_completion_failed") != 1 {
				t.Fatalf("alert keys = %v", fx.notifier.keys)
			}
		})
	}
}

func TestRecoverCompletedArchiveAndClearFailuresRetainCurrentAndAlert(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*orchestratorFixture)
	}{
		{name: "archive", setup: func(fx *orchestratorFixture) { fx.store.archiveErr = errors.New("archive") }},
		{name: "clear", setup: func(fx *orchestratorFixture) { fx.store.clearErr = errors.New("clear") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newOrchestratorFixture(t)
			state := positiveState(t, PhaseCompleted)
			state.DBCompleted = true
			fx.store.current = &state
			tt.setup(fx)
			if err := fx.orchestrator.Recover(context.Background()); err == nil {
				t.Fatal("Recover() error = nil")
			}
			if fx.store.current == nil || fx.store.current.Phase != PhaseCompleted {
				t.Fatalf("current = %#v", fx.store.current)
			}
			if countAlertSuffix(fx.notifier.keys, "database_completion_failed") != 1 {
				t.Fatalf("alert keys = %v", fx.notifier.keys)
			}
		})
	}
}

func TestRecoverConsolidationFailurePathsAlert(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*orchestratorFixture, *RunState)
		want  string
	}{
		{name: "claim prepare", want: "builder_claim_prepare_failed", setup: func(fx *orchestratorFixture, state *RunState) {
			fx.chain.prepareClaimErr = map[string]error{"0xbuilder1": errors.New("secret claim")}
		}},
		{name: "builder balance", want: "builder_balance_failed", setup: func(fx *orchestratorFixture, state *RunState) {
			state.Builders[0].Claim.Phase = ActionAccepted
			fx.chain.balanceErr = map[string]error{"0xbuilder1": errors.New("secret balance")}
		}},
		{name: "sweep prepare", want: "builder_sweep_prepare_failed", setup: func(fx *orchestratorFixture, state *RunState) {
			state.Builders[0].Claim.Phase = ActionAccepted
			fx.chain.balances["0xbuilder1"] = decimal.NewFromInt(1)
			fx.chain.prepareSendErr = map[string]error{"0xbuilder1": errors.New("secret sweep")}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newOrchestratorFixture(t)
			state := positiveState(t, PhaseConsolidating)
			state.Builders = []BuilderProgress{{Name: "one", Address: "0xbuilder1"}}
			fx.store.current = &state
			fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.NewFromInt(2)}
			tt.setup(fx, &state)
			_ = fx.orchestrator.Recover(context.Background())
			if countAlertSuffix(fx.notifier.keys, tt.want) != 1 {
				t.Fatalf("alert keys = %v", fx.notifier.keys)
			}
		})
	}
}

func actionBoundary(kind string, phase ActionPhase) func(RunState) bool {
	return func(state RunState) bool {
		if kind == "payout" {
			return state.FinalPayout != nil && state.FinalPayout.Phase == phase
		}
		for _, builder := range state.Builders {
			var progress ActionProgress
			if kind == "claim" {
				progress = builder.Claim
			} else {
				progress = builder.Sweep
			}
			if progress.Phase == phase {
				return true
			}
		}
		return false
	}
}

func countAlertSuffix(keys []string, suffix string) int {
	count := 0
	for _, key := range keys {
		if strings.HasSuffix(key, ":"+suffix) || key == suffix {
			count++
		}
	}
	return count
}

func filterEvents(events, wanted []string) []string {
	set := make(map[string]struct{}, len(wanted))
	for _, event := range wanted {
		set[event] = struct{}{}
	}
	var got []string
	for _, event := range events {
		if _, ok := set[event]; ok {
			got = append(got, event)
		}
	}
	return got
}
