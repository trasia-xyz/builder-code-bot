package funding_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hyperliquid-builder-code-bot/internal/dev/hyperliquidmock"
	"hyperliquid-builder-code-bot/internal/funding"
	"hyperliquid-builder-code-bot/internal/hyperliquid"
	httpclient "hyperliquid-builder-code-bot/internal/hyperliquid/client"
	"hyperliquid-builder-code-bot/internal/hyperliquid/exchange"
	"hyperliquid-builder-code-bot/internal/hyperliquid/info"
	"hyperliquid-builder-code-bot/internal/hyperliquid/signing"
	"hyperliquid-builder-code-bot/internal/secret"
	"hyperliquid-builder-code-bot/internal/state"

	"github.com/shopspring/decimal"
)

const (
	integrationKey1      = "0x0000000000000000000000000000000000000000000000000000000000000001"
	integrationKey2      = "0x0000000000000000000000000000000000000000000000000000000000000002"
	integrationKey3      = "0x0000000000000000000000000000000000000000000000000000000000000003"
	integrationRecipient = "0x00000000000000000000000000000000000000aa"
)

func TestIntegrationFundingFlow(t *testing.T) {
	for _, test := range []struct {
		name            string
		ambiguousPayout bool
	}{
		{name: "accepted payout"},
		{name: "ambiguous applied payout confirmed by balance", ambiguousPayout: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newIntegrationEnvironment(t, test.ambiguousPayout)
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
			if claims != 2 || sweeps != 2 || payouts != 1 {
				t.Fatalf("claims=%d sweeps=%d payouts=%d", claims, sweeps, payouts)
			}
		})
	}
}

type integrationEnvironment struct {
	server       *hyperliquidmock.Server
	repository   *integrationRepository
	store        *state.Store
	orchestrator *funding.Orchestrator
	info         *info.Client
	settlement   string
	dataDir      string
	payoutAmount decimal.Decimal
}

func newIntegrationEnvironment(t *testing.T, ambiguousPayout bool) *integrationEnvironment {
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
	server.SetClaimReward(addresses[0], "USDC:0", "1")
	server.SetSpotBalance(addresses[1], "USDC:0", "1")
	server.SetClaimReward(addresses[1], "USDC:0", "1")
	server.SetSpotBalance(addresses[2], "USDC:0", "0")
	server.SetSpotBalance(integrationRecipient, "USDC:0", "0")

	repository := &integrationRepository{records: []funding.Record{
		{ID: 2, PeriodStartAt: 200, Amount: "2.000000000000000001"},
		{ID: 1, PeriodStartAt: 100, Amount: "1.123456000000000000"},
	}}
	dataDir := filepath.Join(t.TempDir(), "data")
	store := state.NewStore(dataDir)
	chain := &integrationChain{
		info: info.New(transport), exchange: exchangeClient, server: server,
		settlement: addresses[2], ambiguousPayout: ambiguousPayout,
	}
	clock := fixedIntegrationClock{now: time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)}
	orchestrator, err := funding.NewOrchestrator(funding.OrchestratorConfig{
		Repository: repository, Store: store, Chain: chain,
		Builders:   []string{addresses[0], addresses[1]},
		Settlement: addresses[2], Recipient: integrationRecipient,
		Clock: clock, Nonce: signing.NewNonceGenerator(), Sleeper: noWaitSleeper{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &integrationEnvironment{
		server: server, repository: repository, store: store, orchestrator: orchestrator,
		info: info.New(transport), settlement: addresses[2], dataDir: dataDir,
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
	info            *info.Client
	exchange        *exchange.Client
	server          *hyperliquidmock.Server
	settlement      string
	ambiguousPayout bool
}

func (c *integrationChain) CanonicalUSDC(ctx context.Context) (info.Token, error) {
	return c.info.CanonicalUSDC(ctx)
}
func (c *integrationChain) SpotBalance(ctx context.Context, address string, token info.Token) (info.SpotBalanceAmounts, error) {
	return c.info.SpotBalance(ctx, address, token)
}
func (c *integrationChain) PrepareClaim(address string, nonce uint64) (exchange.PreparedAction, error) {
	return c.exchange.PrepareClaim(address, nonce)
}
func (c *integrationChain) PrepareSpotSend(address, destination string, token info.Token, amount decimal.Decimal, nonce uint64) (exchange.PreparedAction, error) {
	return c.exchange.PrepareSpotSend(address, destination, token, amount, nonce)
}
func (c *integrationChain) Submit(ctx context.Context, action exchange.PreparedAction) (exchange.SubmitResult, error) {
	if c.ambiguousPayout && strings.EqualFold(action.Signer, c.settlement) {
		c.server.FailNextExchange(hyperliquidmock.FailureAmbiguousApplied)
		c.ambiguousPayout = false
	}
	return c.exchange.Submit(ctx, action)
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
