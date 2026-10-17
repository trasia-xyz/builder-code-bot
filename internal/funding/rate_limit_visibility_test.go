package funding

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"builder-code-bot/internal/hyperliquid/info"
)

func TestRateLimitVisibilityWaitsForPostActionLowValue(t *testing.T) {
	fx := newRateLimitVisibilityFixture(200, 200, 199)

	fx.orchestrator.observeUserRateLimits(context.Background(), &fx.report)

	if fx.chain.calls != 3 {
		t.Fatalf("UserRateLimit() calls = %d, want 3", fx.chain.calls)
	}
	if len(fx.sleeper.delays) != 2 ||
		fx.sleeper.delays[0] != userRateLimitVisibilityInterval ||
		fx.sleeper.delays[1] != userRateLimitVisibilityInterval {
		t.Fatalf("visibility delays = %v", fx.sleeper.delays)
	}
	if !fx.report.SettlementRateLimitObserved ||
		fx.report.SettlementRateLimitQueryFailed ||
		fx.report.SettlementRateLimitVisibilityUnconfirmed ||
		fx.report.SettlementRateLimitRemaining != 199 {
		t.Fatalf("settlement rate limit report = %#v", fx.report)
	}
	if !rateLimitTestContains(fx.report.Warnings, "Settlement: Rate Limit 仅剩 199") {
		t.Fatalf("warnings = %#v", fx.report.Warnings)
	}
	if !rateLimitTestContains(fx.notifier.keys, "user_rate_limit_low:0xsettlement") {
		t.Fatalf("alert keys = %#v", fx.notifier.keys)
	}
	if len(fx.notifier.messages) != 1 ||
		!strings.Contains(fx.notifier.messages[0], "requests remaining: 199") {
		t.Fatalf("alert messages = %#v", fx.notifier.messages)
	}
}

func TestRateLimitVisibilityDoesNotPresentPersistentBoundaryAsConfirmed(t *testing.T) {
	fx := newRateLimitVisibilityFixture(200, 200, 200)

	fx.orchestrator.observeUserRateLimits(context.Background(), &fx.report)

	if fx.chain.calls != userRateLimitVisibilityAttempts {
		t.Fatalf("UserRateLimit() calls = %d, want %d", fx.chain.calls, userRateLimitVisibilityAttempts)
	}
	if !fx.report.SettlementRateLimitObserved ||
		fx.report.SettlementRateLimitQueryFailed ||
		!fx.report.SettlementRateLimitVisibilityUnconfirmed ||
		fx.report.SettlementRateLimitRemaining != 200 {
		t.Fatalf("settlement rate limit report = %#v", fx.report)
	}
	warning := "Settlement: Rate Limit 动作后计数未确认刷新（最低观测值 200）"
	if !rateLimitTestContains(fx.report.Warnings, warning) {
		t.Fatalf("warnings = %#v", fx.report.Warnings)
	}
	if !rateLimitTestContains(fx.notifier.keys, "user_rate_limit_low:0xsettlement") {
		t.Fatalf("alert keys = %#v", fx.notifier.keys)
	}
	if len(fx.notifier.messages) != 1 ||
		!strings.Contains(fx.notifier.messages[0], "post-action visibility was not confirmed") ||
		!strings.Contains(fx.notifier.messages[0], "lowest observed remaining: 200") {
		t.Fatalf("alert messages = %#v", fx.notifier.messages)
	}
}

func TestRateLimitVisibilityAvoidsExtraQueriesOutsideConfirmationWindow(t *testing.T) {
	for _, test := range []struct {
		name      string
		remaining uint64
		warning   string
		alert     bool
	}{
		{
			name:      "clearly healthy",
			remaining: userRateLimitAlertThreshold + userRateLimitVisibilityActionWindow,
		},
		{
			name: "already low", remaining: userRateLimitAlertThreshold - 1,
			warning: "Settlement: Rate Limit 仅剩 199", alert: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fx := newRateLimitVisibilityFixture(test.remaining)

			fx.orchestrator.observeUserRateLimits(context.Background(), &fx.report)

			if fx.chain.calls != 1 || len(fx.sleeper.delays) != 0 {
				t.Fatalf("calls = %d, delays = %v", fx.chain.calls, fx.sleeper.delays)
			}
			if fx.report.SettlementRateLimitRemaining != test.remaining {
				t.Fatalf("remaining = %d, want %d", fx.report.SettlementRateLimitRemaining, test.remaining)
			}
			if fx.report.SettlementRateLimitVisibilityUnconfirmed {
				t.Fatal("visibility unexpectedly unconfirmed")
			}
			if test.warning != "" && !rateLimitTestContains(fx.report.Warnings, test.warning) {
				t.Fatalf("warnings = %#v", fx.report.Warnings)
			}
			if got := len(fx.notifier.keys) > 0; got != test.alert {
				t.Fatalf("alerted = %t, want %t; keys = %#v", got, test.alert, fx.notifier.keys)
			}
		})
	}
}

func TestRateLimitVisibilityDoesNotRetryWithoutCurrentRunAction(t *testing.T) {
	fx := newRateLimitVisibilityFixture(200)
	fx.report.submittedAccounts = nil

	fx.orchestrator.observeUserRateLimits(context.Background(), &fx.report)

	if fx.chain.calls != 1 || fx.report.SettlementRateLimitVisibilityUnconfirmed ||
		fx.report.SettlementRateLimitRemaining != 200 || len(fx.report.Warnings) != 0 {
		t.Fatalf("calls=%d report=%#v", fx.chain.calls, fx.report)
	}
}

func TestRecoveredPayoutRetainsRateLimitVisibilityBoundary(t *testing.T) {
	fx := newRateLimitVisibilityFixture(200, 200, 199)
	fx.report.submittedAccounts = nil
	state := &RunState{
		Targets: []PayoutTargetState{
			{
				Kind: PayoutKindReferral, Phase: PhasePayoutSubmitting,
				Manifest: Manifest{Settlement: "0xsettlement"},
				Payout:   &PayoutJournal{},
			},
		},
	}
	markRecoveredPayoutSubmission(&fx.report, state, &state.Targets[0], StateLoadMetadata{})

	fx.orchestrator.observeUserRateLimits(context.Background(), &fx.report)

	if fx.chain.calls != 3 || fx.report.SettlementRateLimitRemaining != 199 ||
		fx.report.SettlementRateLimitVisibilityUnconfirmed {
		t.Fatalf("calls=%d report=%#v", fx.chain.calls, fx.report)
	}
}

func TestOnlyBackupPreparedPayoutMayHaveBeenSubmitted(t *testing.T) {
	state := &RunState{
		Targets: []PayoutTargetState{
			{
				Kind: PayoutKindReferral, Phase: PhasePayoutPrepared,
				Manifest: Manifest{Settlement: "0xsettlement"},
				Payout:   &PayoutJournal{},
			},
		},
	}
	primary := newRunReport(
		time.Unix(0, 0), TriggerUTC, ReportExecution{}, "mainnet",
		nil, nil, "0xsettlement", "0xrecipient",
	)
	markRecoveredPayoutSubmission(&primary, state, &state.Targets[0], StateLoadMetadata{})
	if primary.submitted("0xsettlement") {
		t.Fatal("primary payout_prepared was marked as submitted")
	}
	backup := newRunReport(
		time.Unix(0, 0), TriggerUTC, ReportExecution{}, "mainnet",
		nil, nil, "0xsettlement", "0xrecipient",
	)
	markRecoveredPayoutSubmission(
		&backup, state, &state.Targets[0], StateLoadMetadata{RecoveredFromBackup: true},
	)
	if !backup.submitted("0xsettlement") {
		t.Fatal("backup payout_prepared was not marked as possibly submitted")
	}
}

func TestRateLimitVisibilityKeepsLowestNearThresholdObservation(t *testing.T) {
	fx := newRateLimitVisibilityFixture(205, 203, 201)

	fx.orchestrator.observeUserRateLimits(context.Background(), &fx.report)

	if fx.chain.calls != userRateLimitVisibilityAttempts ||
		fx.report.SettlementRateLimitRemaining != 201 ||
		!fx.report.SettlementRateLimitVisibilityUnconfirmed {
		t.Fatalf("calls = %d, remaining = %d", fx.chain.calls, fx.report.SettlementRateLimitRemaining)
	}
	if !rateLimitTestContains(
		fx.report.Warnings,
		"Settlement: Rate Limit 动作后计数未确认刷新（最低观测值 201）",
	) {
		t.Fatalf("warnings = %#v", fx.report.Warnings)
	}
}

func TestRateLimitVisibilityKeepsInitialObservationWhenRetryFails(t *testing.T) {
	fx := newRateLimitVisibilityFixture(200)
	fx.chain.responses = append(fx.chain.responses, rateLimitVisibilityResponse{err: errors.New("info unavailable")})

	fx.orchestrator.observeUserRateLimits(context.Background(), &fx.report)

	if fx.chain.calls != 2 || !fx.report.SettlementRateLimitObserved ||
		fx.report.SettlementRateLimitQueryFailed ||
		!fx.report.SettlementRateLimitVisibilityUnconfirmed ||
		fx.report.SettlementRateLimitRemaining != 200 {
		t.Fatalf("calls = %d, report = %#v", fx.chain.calls, fx.report)
	}
	if !rateLimitTestContains(
		fx.report.Warnings,
		"Settlement: Rate Limit 动作后计数未确认刷新（最低观测值 200）",
	) {
		t.Fatalf("warnings = %#v", fx.report.Warnings)
	}
}

type rateLimitVisibilityFixture struct {
	orchestrator *Orchestrator
	chain        *rateLimitVisibilityChain
	sleeper      *rateLimitVisibilitySleeper
	notifier     *rateLimitVisibilityNotifier
	report       RunReport
}

func newRateLimitVisibilityFixture(remaining ...uint64) rateLimitVisibilityFixture {
	responses := make([]rateLimitVisibilityResponse, len(remaining))
	for index, value := range remaining {
		responses[index].remaining = value
	}
	chain := &rateLimitVisibilityChain{responses: responses}
	sleeper := &rateLimitVisibilitySleeper{}
	notifier := &rateLimitVisibilityNotifier{}
	orchestrator := &Orchestrator{
		chain: chain, settlement: "0xsettlement", sleeper: sleeper, notifier: notifier,
		lowLimits: make(map[string]struct{}),
	}
	report := newRunReport(
		time.Unix(0, 0), TriggerUTC, ReportExecution{}, "mainnet",
		nil, nil, "0xsettlement", "0xrecipient",
	)
	report.markSubmitted("0xsettlement")
	return rateLimitVisibilityFixture{
		orchestrator: orchestrator, chain: chain, sleeper: sleeper,
		notifier: notifier, report: report,
	}
}

type rateLimitVisibilityResponse struct {
	remaining uint64
	err       error
}

type rateLimitVisibilityChain struct {
	Chain
	responses []rateLimitVisibilityResponse
	calls     int
}

func (c *rateLimitVisibilityChain) UserRateLimit(
	context.Context,
	string,
) (info.UserRateLimit, error) {
	c.calls++
	if len(c.responses) == 0 {
		return info.UserRateLimit{}, errors.New("unexpected rate-limit query")
	}
	response := c.responses[0]
	c.responses = c.responses[1:]
	if response.err != nil {
		return info.UserRateLimit{}, response.err
	}
	const cap = uint64(10_000)
	return info.UserRateLimit{NRequestsUsed: cap - response.remaining, NRequestsCap: cap}, nil
}

type rateLimitVisibilitySleeper struct{ delays []time.Duration }

func (s *rateLimitVisibilitySleeper) Sleep(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.delays = append(s.delays, delay)
	return nil
}

type rateLimitVisibilityNotifier struct {
	keys     []string
	messages []string
}

func (n *rateLimitVisibilityNotifier) Alert(
	_ context.Context,
	key string,
	_ AlertSeverity,
	message string,
) {
	n.keys = append(n.keys, key)
	n.messages = append(n.messages, message)
}

func (*rateLimitVisibilityNotifier) Report(context.Context, RunReport) {}

func rateLimitTestContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
