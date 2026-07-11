package funding

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
)

func TestRecoverRetriesBuilderActionThatFailedBeforePreparation(t *testing.T) {
	tests := []struct {
		name    string
		builder BuilderProgress
		balance decimal.Decimal
		want    string
	}{
		{
			name: "claim",
			builder: BuilderProgress{
				Name: "one", Address: "0xbuilder1",
				Claim: ActionProgress{Phase: ActionRejected},
			},
			want: "submit:claimRewards:0xbuilder1",
		},
		{
			name: "sweep",
			builder: BuilderProgress{
				Name: "one", Address: "0xbuilder1",
				Claim: ActionProgress{Phase: ActionAccepted},
				Sweep: ActionProgress{Phase: ActionRejected},
			},
			balance: decimal.NewFromInt(1),
			want:    "submit:spotSend:0xbuilder1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fx := newOrchestratorFixture(t)
			state := positiveState(t, PhaseBlocked)
			state.Builders = []BuilderProgress{test.builder}
			fx.store.current = &state
			fx.chain.balances = map[string]decimal.Decimal{
				"0xbuilder1":   test.balance,
				"0xsettlement": decimal.NewFromInt(2),
			}

			if err := fx.orchestrator.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !slicesContain(fx.chain.events, test.want) {
				t.Fatalf("events = %v, want %q", fx.chain.events, test.want)
			}
		})
	}
}

func TestRecoverDoesNotRetryExplicitlyRejectedPreparedAction(t *testing.T) {
	fx := newOrchestratorFixture(t)
	state := positiveState(t, PhaseBlocked)
	claim := prepared("claimRewards", "0xbuilder1", "", "", 100)
	state.Builders = []BuilderProgress{{
		Name: "one", Address: "0xbuilder1",
		Claim: ActionProgress{Phase: ActionRejected, Prepared: &claim, SubmitAttempts: 1},
	}}
	fx.store.current = &state
	fx.chain.balances = map[string]decimal.Decimal{
		"0xbuilder1":   decimal.Zero,
		"0xsettlement": decimal.Zero,
	}

	if err := fx.orchestrator.Recover(context.Background()); err == nil {
		t.Fatal("Recover() error = nil, want underfunded run to remain blocked")
	}
	if slicesContain(fx.chain.events, "submit:claimRewards:0xbuilder1") {
		t.Fatalf("explicitly rejected claim was retried: %v", fx.chain.events)
	}
}
