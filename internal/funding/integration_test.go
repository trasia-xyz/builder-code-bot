package funding_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"builder-code-bot/internal/dev/hyperliquidmock"
	"builder-code-bot/internal/funding"
	"builder-code-bot/internal/hyperliquid"
	httpclient "builder-code-bot/internal/hyperliquid/client"
	"builder-code-bot/internal/hyperliquid/exchange"
	"builder-code-bot/internal/hyperliquid/info"
	"builder-code-bot/internal/hyperliquid/signing"
	"builder-code-bot/internal/secret"
	"builder-code-bot/internal/state"

	"github.com/shopspring/decimal"
)

const (
	integrationKey1      = "0x0000000000000000000000000000000000000000000000000000000000000001"
	integrationKey2      = "0x0000000000000000000000000000000000000000000000000000000000000002"
	integrationKey3      = "0x0000000000000000000000000000000000000000000000000000000000000003"
	integrationRecipient = "0x00000000000000000000000000000000000000aa"
	integrationReferral  = "0x00000000000000000000000000000000000000ab"
)

func TestIntegrationFundingFlow(t *testing.T) {
	for _, test := range []struct {
		name            string
		ambiguousPayout bool
		ambiguousSweep  bool
		wantSweeps      int
	}{
		{name: "accepted payout", wantSweeps: 2},
		{name: "ambiguous unapplied internal sweep is retried", ambiguousSweep: true, wantSweeps: 3},
		{name: "ambiguous applied payout confirmed by balance", ambiguousPayout: true, wantSweeps: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newIntegrationEnvironment(t, test.ambiguousPayout, test.ambiguousSweep)
			if err := env.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
				t.Fatal(err)
			}
			consolidation, err := env.store.LoadDailyConsolidation(context.Background(), "2026-07-12")
			if err != nil || consolidation == nil || len(consolidation.Builders) != 2 {
				t.Fatalf("daily consolidation = %#v, error = %v", consolidation, err)
			}
			for index, builder := range consolidation.Builders {
				wantStatus := funding.ReportStepSuccess
				wantSummary := "资金已归集"
				if test.ambiguousSweep && index == 0 {
					wantStatus = funding.ReportStepWarning
					wantSummary = "此前响应未知"
				}
				if builder.SweepStatus != wantStatus ||
					!strings.Contains(builder.SweepSummary, wantSummary) {
					t.Fatalf("builder sweep result = %#v", builder)
				}
			}
			if err := env.orchestrator.Run(context.Background(), funding.TriggerRunOnStart); err != nil {
				t.Fatal(err)
			}
			if !env.repository.completed {
				t.Fatal("repository was not completed")
			}
			current, _, err := env.store.LoadWithMetadata(context.Background())
			if err != nil || current != nil {
				t.Fatalf("current state = %#v, error = %v", current, err)
			}
			entries, err := os.ReadDir(filepath.Join(env.dataDir, "history"))
			if err != nil || len(entries) != 1 {
				t.Fatalf("history entries = %d, error = %v", len(entries), err)
			}
			if got := env.balance(t, integrationRecipient); !got.Equal(env.payoutAmount) {
				t.Fatalf("recipient balance = %s, want %s", got, env.payoutAmount)
			}
			if len(env.notifier.reports) != 1 {
				t.Fatalf("final reports = %d, want 1", len(env.notifier.reports))
			}
			finalBalance := env.notifier.reports[0].SettlementBalance
			wantFinal := decimal.NewFromInt(4).Sub(env.payoutAmount).String()
			if !finalBalance.Observed || finalBalance.PostPayoutUnconfirmed ||
				finalBalance.Total != wantFinal || finalBalance.Available != wantFinal {
				t.Fatalf("final settlement balance = %#v, want %s", finalBalance, wantFinal)
			}

			var claims, sweeps, payouts int
			for _, request := range env.server.Requests() {
				switch {
				case request.ActionType == "claimRewards":
					claims++
				case request.ActionType == "spotSend" && strings.EqualFold(request.Destination, env.settlement):
					sweeps++
				case request.ActionType == "spotSend" && strings.EqualFold(request.Destination, integrationRecipient):
					payouts++
				}
			}
			if claims != 2 || sweeps != test.wantSweeps || payouts != 1 {
				t.Fatalf("claims=%d sweeps=%d payouts=%d", claims, sweeps, payouts)
			}
			for _, account := range append(append([]string(nil), env.builders...), env.settlement) {
				if pending := env.server.PendingSpotBalanceSnapshots(account, "USDC:0"); pending != 0 {
					t.Fatalf("pending stale balance snapshots for %s = %d", account, pending)
				}
				if reads := env.server.AuthoritativeSpotBalanceReadsAfterSnapshots(
					account, "USDC:0",
				); reads < 2 {
					t.Fatalf("authoritative balance reads for %s = %d, want at least 2", account, reads)
				}
			}
		})
	}
}

func TestIntegrationDualPayoutFlow(t *testing.T) {
	env := newIntegrationEnvironment(t, false, false)
	env.referralRepo.records = []funding.Record{
		{ID: 1, PeriodStartAt: 300, Amount: "0.250000000000000001"},
	}
	if err := env.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := env.orchestrator.Run(context.Background(), funding.TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	if !env.repository.completed || !env.referralRepo.completed {
		t.Fatalf("points completed=%t referral completed=%t",
			env.repository.completed, env.referralRepo.completed)
	}
	if got := env.balance(t, integrationRecipient); !got.Equal(env.payoutAmount) {
		t.Fatalf("points recipient balance = %s, want %s", got, env.payoutAmount)
	}
	referralAmount := decimal.RequireFromString("0.250001")
	if got := env.balance(t, integrationReferral); !got.Equal(referralAmount) {
		t.Fatalf("referral recipient balance = %s, want %s", got, referralAmount)
	}

	var payouts []hyperliquidmock.RecordedRequest
	for _, request := range env.server.Requests() {
		if request.ActionType == "spotSend" &&
			(strings.EqualFold(request.Destination, integrationRecipient) ||
				strings.EqualFold(request.Destination, integrationReferral)) {
			payouts = append(payouts, request)
		}
	}
	if len(payouts) != 2 ||
		!strings.EqualFold(payouts[0].Destination, integrationRecipient) ||
		!strings.EqualFold(payouts[1].Destination, integrationReferral) {
		t.Fatalf("payout requests = %#v", payouts)
	}
	if len(env.notifier.reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(env.notifier.reports))
	}
	report := env.notifier.reports[0]
	wantTotal := env.payoutAmount.Add(referralAmount)
	wantFinal := decimal.NewFromInt(4).Sub(wantTotal)
	if report.PayoutTotal != wantTotal.String() || report.RecordCount != 3 ||
		!report.SettlementBalance.Observed || report.SettlementBalance.Total != wantFinal.String() ||
		len(report.Payouts) != 2 ||
		report.Payouts[0].Payout.Status != funding.ReportStepSuccess ||
		report.Payouts[1].Payout.Status != funding.ReportStepSuccess {
		t.Fatalf("dual payout report = %#v", report)
	}
}

func TestIntegrationZeroClaimableIsObservedWithoutClaimSubmission(t *testing.T) {
	env := newIntegrationEnvironment(t, false, false)
	env.server.SetClaimReward(env.builders[0], "USDC:0", "0")

	if err := env.orchestrator.RunDailyConsolidation(context.Background()); err != nil {
		t.Fatal(err)
	}
	consolidation, err := env.store.LoadDailyConsolidation(context.Background(), "2026-07-12")
	if err != nil || consolidation == nil || len(consolidation.Builders) != 2 {
		t.Fatalf("daily consolidation = %#v, error = %v", consolidation, err)
	}
	builder := consolidation.Builders[0]
	if !builder.ClaimableObserved || builder.ClaimableUSDC != "0" ||
		builder.ClaimableQueryAttempts != 1 || builder.ClaimAttempts != 0 ||
		builder.ClaimStatus != funding.ReportStepSkipped ||
		builder.SweepAmount != "0.5" || builder.SweepStatus != funding.ReportStepSuccess {
		t.Fatalf("zero claimable builder = %#v", builder)
	}

	claims := 0
	for _, request := range env.server.Requests() {
		if request.ActionType == "claimRewards" {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("claim submissions = %d, want only the positive builder claim", claims)
	}
}

type integrationEnvironment struct {
	server       *hyperliquidmock.Server
	repository   *integrationRepository
	referralRepo *integrationRepository
	store        *state.Store
	orchestrator *funding.Orchestrator
	notifier     *integrationNotifier
	info         *info.Client
	settlement   string
	builders     []string
	dataDir      string
	payoutAmount decimal.Decimal
}

func newIntegrationEnvironment(
	t *testing.T,
	ambiguousPayout, ambiguousSweep bool,
) *integrationEnvironment {
	t.Helper()
	server := hyperliquidmock.New(t)
	signers := make(map[string]signing.PrivateKey)
	addresses := make([]string, 0, 3)
	for _, raw := range []string{integrationKey1, integrationKey2, integrationKey3} {
		key, err := signing.ParsePrivateKey(secret.NewString(raw))
		if err != nil {
			t.Fatal(err)
		}
		address, err := key.Address()
		if err != nil {
			t.Fatal(err)
		}
		signers[address] = key
		addresses = append(addresses, address)
	}
	transport, err := httpclient.New(httpclient.Config{Network: hyperliquid.NetworkTestnet, BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	exchangeClient, err := exchange.New(transport, hyperliquid.NetworkTestnet, signers)
	if err != nil {
		t.Fatal(err)
	}
	server.SetSpotBalance(addresses[0], "USDC:0", "0.5")
	server.SetClaimReward(addresses[0], "USDC:0", "1.25")
	server.SetSpotBalance(addresses[1], "USDC:0", "1")
	server.SetClaimReward(addresses[1], "USDC:0", "1.25")
	server.SetSpotBalance(addresses[2], "USDC:0", "0")
	server.SetSpotBalance(integrationRecipient, "USDC:0", "0")
	server.SetSpotBalance(integrationReferral, "USDC:0", "0")

	repository := &integrationRepository{records: []funding.Record{
		{ID: 2, PeriodStartAt: 200, Amount: "2.000000000000000001"},
		{ID: 1, PeriodStartAt: 100, Amount: "1.123456000000000000"},
	}}
	referralRepository := &integrationRepository{}
	dataDir := filepath.Join(t.TempDir(), "data")
	store := state.NewStore(dataDir)
	chain := &integrationChain{
		info: info.New(transport), exchange: exchangeClient, server: server,
		settlement: addresses[2], ambiguousPayout: ambiguousPayout,
		ambiguousSweep:    ambiguousSweep,
		delayedVisibility: true,
		token: info.Token{
			Name: "USDC", TokenID: "0", Index: 0, WeiDecimals: 6, WireToken: "USDC:0",
		},
	}
	clock := fixedIntegrationClock{now: time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)}
	notifier := &integrationNotifier{}
	orchestrator, err := funding.NewOrchestrator(funding.OrchestratorConfig{
		PointsRebateRepository: repository, ReferralRepository: referralRepository,
		Store: store, DailyConsolidations: store, Chain: chain,
		Builders:   []string{addresses[0], addresses[1]},
		Settlement: addresses[2], PointsRebateRecipient: integrationRecipient,
		ReferralRecipient: integrationReferral,
		Notifier:          notifier, Clock: clock, Nonce: signing.NewNonceGenerator(), Sleeper: noWaitSleeper{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &integrationEnvironment{
		server: server, repository: repository, referralRepo: referralRepository,
		store: store, orchestrator: orchestrator,
		notifier: notifier,
		info:     info.New(transport), settlement: addresses[2], dataDir: dataDir,
		builders:     append([]string(nil), addresses[:2]...),
		payoutAmount: decimal.RequireFromString("3.123457"),
	}
}

func (e *integrationEnvironment) balance(t *testing.T, address string) decimal.Decimal {
	t.Helper()
	token := info.Token{Name: "USDC", TokenID: "0", Index: 0, WeiDecimals: 6, WireToken: "USDC:0"}
	balance, err := e.info.SpotBalance(context.Background(), address, token)
	if err != nil {
		t.Fatal(err)
	}
	return balance.Available
}

type integrationChain struct {
	info              *info.Client
	exchange          *exchange.Client
	server            *hyperliquidmock.Server
	settlement        string
	ambiguousPayout   bool
	ambiguousSweep    bool
	delayedVisibility bool
	token             info.Token
}

func (c *integrationChain) CanonicalUSDC(ctx context.Context) (info.Token, error) {
	return c.info.CanonicalUSDC(ctx)
}
func (c *integrationChain) ClaimableUSDC(ctx context.Context, address string, token info.Token) (decimal.Decimal, error) {
	return c.info.ClaimableUSDC(ctx, address, token)
}
func (c *integrationChain) SpotBalance(ctx context.Context, address string, token info.Token) (info.SpotBalanceAmounts, error) {
	return c.info.SpotBalance(ctx, address, token)
}
func (c *integrationChain) UserRateLimit(ctx context.Context, address string) (info.UserRateLimit, error) {
	return c.info.UserRateLimit(ctx, address)
}
func (c *integrationChain) PrepareClaim(address string, nonce uint64) (exchange.PreparedAction, error) {
	return c.exchange.PrepareClaim(address, nonce)
}
func (c *integrationChain) PrepareSpotSend(address, destination string, token info.Token, amount decimal.Decimal, nonce uint64) (exchange.PreparedAction, error) {
	return c.exchange.PrepareSpotSend(address, destination, token, amount, nonce)
}
func (c *integrationChain) Submit(ctx context.Context, action exchange.PreparedAction) (exchange.SubmitResult, error) {
	var before info.SpotBalanceAmounts
	if c.delayedVisibility {
		var err error
		before, err = c.info.SpotBalance(ctx, action.Signer, c.token)
		if err != nil {
			return exchange.SubmitResult{}, err
		}
	}
	ambiguousApplied := false
	if c.ambiguousSweep && action.Kind == "spotSend" &&
		!strings.EqualFold(action.Signer, c.settlement) {
		c.server.FailNextExchange(hyperliquidmock.FailureAmbiguous)
		c.ambiguousSweep = false
	}
	if c.ambiguousPayout && strings.EqualFold(action.Signer, c.settlement) {
		c.server.FailNextExchange(hyperliquidmock.FailureAmbiguousApplied)
		c.ambiguousPayout = false
		ambiguousApplied = true
	}
	result, err := c.exchange.Submit(ctx, action)
	observeAfterSubmit := action.Kind == "claimRewards" ||
		strings.EqualFold(action.Signer, c.settlement)
	if c.delayedVisibility && observeAfterSubmit && (result.Accepted || ambiguousApplied) {
		hold := before.Total.Sub(before.Available).String()
		for range 2 {
			c.server.QueueSpotBalanceSnapshot(
				action.Signer, c.token.WireToken, before.Total.String(), hold,
			)
		}
	}
	return result, err
}

type integrationNotifier struct {
	reports []funding.RunReport
}

func (*integrationNotifier) Alert(context.Context, string, funding.AlertSeverity, string) {}

func (n *integrationNotifier) Report(_ context.Context, report funding.RunReport) {
	n.reports = append(n.reports, report)
}

type integrationRepository struct {
	records   []funding.Record
	completed bool
}

func (r *integrationRepository) ListPending(context.Context) ([]funding.Record, error) {
	return append([]funding.Record(nil), r.records...), nil
}
func (r *integrationRepository) Complete(context.Context, []uint64) error {
	r.completed = true
	return nil
}

type fixedIntegrationClock struct{ now time.Time }

func (c fixedIntegrationClock) Now() time.Time { return c.now }

type noWaitSleeper struct{}

func (noWaitSleeper) Sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }
