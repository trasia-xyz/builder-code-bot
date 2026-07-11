package funding

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

type recordingSleeper struct {
	calls     int
	durations []time.Duration
	sleep     func(context.Context, time.Duration) error
}

func (s *recordingSleeper) Sleep(ctx context.Context, delay time.Duration) error {
	s.calls++
	s.durations = append(s.durations, delay)
	if s.sleep != nil {
		return s.sleep(ctx, delay)
	}
	return nil
}

type advancingClock struct{ now time.Time }

func (c *advancingClock) Now() time.Time { return c.now }

func (c *advancingClock) Advance(delay time.Duration) { c.now = c.now.Add(delay) }

func TestBuilderBalanceAfterClaimWaitsUntilVisibilityTimeAndReadsOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		phase ActionPhase
	}{
		{name: "accepted", phase: ActionAccepted},
		{name: "unknown", phase: ActionUnknown},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fx := newOrchestratorFixture(t)
			now := time.Date(2026, 7, 11, 1, 2, 3, 0, time.UTC)
			clock := &advancingClock{now: now}
			sleeper := &recordingSleeper{sleep: func(_ context.Context, delay time.Duration) error {
				clock.Advance(delay)
				return nil
			}}
			fx.orchestrator.clock = clock
			fx.orchestrator.sleeper = sleeper
			fx.chain.balanceQueues = map[string][]decimal.Decimal{
				"0xbuilder1": {decimal.NewFromInt(6), decimal.NewFromInt(7)},
			}
			claim := prepared("claimRewards", "0xbuilder1", "", "", uint64(now.Add(-250*time.Millisecond).UnixMilli()))

			balance, err := fx.orchestrator.builderBalanceAfterClaim(context.Background(), "0xbuilder1", fx.chain.token, ActionProgress{
				Phase: test.phase, Prepared: &claim, SubmitAttempts: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !balance.Equal(decimal.NewFromInt(6)) {
				t.Fatalf("balance = %s, want 6", balance)
			}
			if sleeper.calls != 1 || sleeper.durations[0] != 750*time.Millisecond {
				t.Fatalf("visibility sleeps = %v, want 750ms", sleeper.durations)
			}
			if got := countPrefix(fx.chain.events, "balance:0xbuilder1"); got != 1 {
				t.Fatalf("balance queries = %d, want 1", got)
			}
		})
	}
}

func TestBuilderBalanceAfterRejectedOrUnsubmittedClaimReadsImmediately(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		claim ActionProgress
	}{
		{name: "rejected", claim: ActionProgress{Phase: ActionRejected, SubmitAttempts: 1}},
		{name: "unsubmitted", claim: ActionProgress{Phase: ActionPrepared}},
		{name: "missing", claim: ActionProgress{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fx := newOrchestratorFixture(t)
			sleeper := &recordingSleeper{}
			fx.orchestrator.sleeper = sleeper
			fx.chain.balances = map[string]decimal.Decimal{"0xbuilder1": decimal.NewFromInt(1)}
			if test.claim.Phase != "" {
				claim := prepared("claimRewards", "0xbuilder1", "", "", uint64(fx.orchestrator.clock.Now().UnixMilli()))
				test.claim.Prepared = &claim
			}

			if _, err := fx.orchestrator.builderBalanceAfterClaim(context.Background(), "0xbuilder1", fx.chain.token, test.claim); err != nil {
				t.Fatal(err)
			}
			if sleeper.calls != 0 {
				t.Fatalf("sleep calls = %d, want 0", sleeper.calls)
			}
			if got := countPrefix(fx.chain.events, "balance:0xbuilder1"); got != 1 {
				t.Fatalf("balance queries = %d, want 1", got)
			}
		})
	}
}

func TestAcceptedClaimsShareOneSecondVisibilityWindow(t *testing.T) {
	fx := newOrchestratorFixture(t)
	clock := &advancingClock{now: time.Date(2026, 7, 11, 1, 2, 3, 0, time.UTC)}
	sleeper := &recordingSleeper{sleep: func(_ context.Context, delay time.Duration) error {
		clock.Advance(delay)
		return nil
	}}
	fx.orchestrator.clock = clock
	fx.orchestrator.nonce = &fakeNonce{next: uint64(clock.Now().UnixMilli() - 1)}
	fx.orchestrator.sleeper = sleeper
	fx.repo.records = []Record{{ID: 1, Amount: "1"}}
	fx.chain.balances = map[string]decimal.Decimal{
		"0xbuilder1":   decimal.NewFromInt(1),
		"0xbuilder2":   decimal.NewFromInt(1),
		"0xsettlement": decimal.NewFromInt(2),
	}

	if err := fx.orchestrator.RunNew(context.Background(), TriggerUTC); err != nil {
		t.Fatal(err)
	}

	var elapsed time.Duration
	for _, delay := range sleeper.durations {
		elapsed += delay
	}
	if elapsed != time.Second+time.Millisecond {
		t.Fatalf("total visibility wait = %s, want 1.001s; sleeps = %v", elapsed, sleeper.durations)
	}
	for _, address := range []string{"0xbuilder1", "0xbuilder2"} {
		if got := countPrefix(fx.chain.events, "balance:"+address); got != 1 {
			t.Fatalf("balance queries for %s = %d, want 1", address, got)
		}
	}
}

func TestClaimVisibilityWaitHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	fx := newOrchestratorFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	fx.orchestrator.sleeper = &recordingSleeper{sleep: func(context.Context, time.Duration) error {
		cancel()
		return ctx.Err()
	}}
	claim := prepared("claimRewards", "0xbuilder1", "", "", uint64(fx.orchestrator.clock.Now().UnixMilli()))

	_, err := fx.orchestrator.builderBalanceAfterClaim(ctx, "0xbuilder1", fx.chain.token, ActionProgress{
		Phase: ActionAccepted, Prepared: &claim, SubmitAttempts: 1,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("builderBalanceAfterClaim() error = %v, want context.Canceled", err)
	}
	if got := countPrefix(fx.chain.events, "balance:0xbuilder1"); got != 0 {
		t.Fatalf("balance queries after cancellation = %d, want 0", got)
	}
}

func TestRecoverRechecksZeroBalanceBuilder(t *testing.T) {
	fx := newOrchestratorFixture(t)
	clock := &advancingClock{now: time.Date(2026, 7, 11, 1, 2, 3, 0, time.UTC)}
	fx.orchestrator.clock = clock
	fx.orchestrator.nonce = &fakeNonce{next: uint64(clock.Now().UnixMilli() - 1)}
	fx.orchestrator.sleeper = &recordingSleeper{sleep: func(_ context.Context, delay time.Duration) error {
		clock.Advance(delay)
		return nil
	}}
	fx.repo.records = []Record{{ID: 1, Amount: "1"}}
	fx.chain.balances = map[string]decimal.Decimal{
		"0xbuilder1":   decimal.Zero,
		"0xbuilder2":   decimal.Zero,
		"0xsettlement": decimal.Zero,
	}

	if err := fx.orchestrator.RunNew(context.Background(), TriggerUTC); err == nil {
		t.Fatal("RunNew() error = nil, want underfunded run retained for recovery")
	}
	if fx.store.current == nil || fx.store.current.Builders[0].Sweep.Phase != ActionZeroBalance {
		t.Fatalf("initial sweep state = %#v", fx.store.current)
	}

	fx.chain.balances["0xbuilder1"] = decimal.RequireFromString("1.25")
	fx.chain.balances["0xsettlement"] = decimal.NewFromInt(2)
	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := countPrefix(fx.chain.events, "prepare_send:0xbuilder1:0xsettlement:1.25"); got != 1 {
		t.Fatalf("recovered sweep preparations = %d, want 1; events = %v", got, fx.chain.events)
	}
}
