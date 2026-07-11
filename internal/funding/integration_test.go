package funding_test

import (
	"context"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"hyperliquid-builder-code-bot/internal/dev/hyperliquidmock"
	"hyperliquid-builder-code-bot/internal/funding"
	"hyperliquid-builder-code-bot/internal/hyperliquid"
	httpclient "hyperliquid-builder-code-bot/internal/hyperliquid/client"
	"hyperliquid-builder-code-bot/internal/hyperliquid/exchange"
	"hyperliquid-builder-code-bot/internal/hyperliquid/info"
	"hyperliquid-builder-code-bot/internal/hyperliquid/signing"
	"hyperliquid-builder-code-bot/internal/secret"
	"hyperliquid-builder-code-bot/internal/state"
)

const (
	integrationKey1      = "0x0000000000000000000000000000000000000000000000000000000000000001"
	integrationKey2      = "0x0000000000000000000000000000000000000000000000000000000000000002"
	integrationKey3      = "0x0000000000000000000000000000000000000000000000000000000000000003"
	integrationRecipient = "0x00000000000000000000000000000000000000aa"
)

func TestIntegrationHappyPath(t *testing.T) {
	env := newIntegrationEnvironment(t)
	if err := env.orchestrator(env.store).RunNew(context.Background(), funding.TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}

	assertIntegrationCompleted(t, env)
	requests := env.server.Requests()
	var claims, sweeps, payouts int
	wantSweep := map[string]string{
		strings.ToLower(env.builders[0].Address): "1.5",
		strings.ToLower(env.builders[1].Address): "2",
	}
	for _, request := range requests {
		switch {
		case request.ActionType == "claimRewards":
			claims++
		case request.ActionType == "spotSend" && strings.EqualFold(request.Destination, env.settlement):
			sweeps++
			if want := wantSweep[strings.ToLower(request.Signer)]; request.Amount != want {
				t.Fatalf("sweep from %s = %s, want full claimed balance %s", request.Signer, request.Amount, want)
			}
		case request.ActionType == "spotSend" && strings.EqualFold(request.Destination, integrationRecipient):
			payouts++
		}
	}
	if claims != 2 || sweeps != 2 || payouts != 1 {
		t.Fatalf("exchange calls: claims=%d sweeps=%d payouts=%d", claims, sweeps, payouts)
	}
}

func TestIntegrationRejectedClaimDoesNotBlockFundedPayout(t *testing.T) {
	env := newIntegrationEnvironment(t)
	env.server.SetSpotBalance(env.settlement, "USDC:0", env.payoutAmount.String())
	env.server.FailNextExchange(hyperliquidmock.FailureRejected)

	if err := env.orchestrator(env.store).RunNew(context.Background(), funding.TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}

	assertIntegrationCompleted(t, env)
	var payouts int
	for _, request := range env.server.Requests() {
		if request.ActionType == "spotSend" && strings.EqualFold(request.Destination, integrationRecipient) {
			payouts++
		}
	}
	if payouts != 1 {
		t.Fatalf("payouts = %d, want 1 despite rejected builder claim", payouts)
	}
}

func TestIntegrationRecoveryCrashMatrix(t *testing.T) {
	tests := []struct {
		name  string
		match func(funding.RunState) bool
	}{
		{name: "claim submitting", match: func(saved funding.RunState) bool {
			for _, builder := range saved.Builders {
				if builder.Claim.Phase == funding.ActionSubmitting {
					return true
				}
			}
			return false
		}},
		{name: "sweep submitting", match: func(saved funding.RunState) bool {
			for _, builder := range saved.Builders {
				if builder.Sweep.Phase == funding.ActionSubmitting {
					return true
				}
			}
			return false
		}},
		{name: "payout submitting", match: func(saved funding.RunState) bool {
			return saved.Phase == funding.PhasePayoutSubmitting
		}},
		{name: "payout accepted", match: func(saved funding.RunState) bool {
			return saved.Phase == funding.PhasePayoutAccepted
		}},
		{name: "db updating", match: func(saved funding.RunState) bool {
			return saved.Phase == funding.PhaseDBUpdating
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newIntegrationEnvironment(t)
			crashing := &crashStore{StateStore: env.store, match: tt.match}
			err := env.orchestrator(crashing).RunNew(context.Background(), funding.TriggerRunOnStart)
			if !errors.Is(err, errSimulatedCrash) {
				t.Fatalf("RunNew() error = %v, want simulated crash", err)
			}
			if err := env.orchestrator(env.store).Recover(context.Background()); err != nil {
				t.Fatalf("Recover() error = %v", err)
			}
			assertIntegrationCompleted(t, env)
		})
	}
}

func TestIntegrationRecoveryDelayedLedgerVisibility(t *testing.T) {
	env := newIntegrationEnvironment(t, hyperliquidmock.WithLedgerDelay(1))

	// Inject ambiguity immediately before the payout through a store callback.
	injecting := &callbackStore{StateStore: env.store, after: func(saved funding.RunState) {
		if saved.Phase == funding.PhasePayoutSubmitting {
			env.server.FailNextExchange(hyperliquidmock.FailureAmbiguousApplied)
		}
	}}
	if err := env.orchestrator(injecting).RunNew(context.Background(), funding.TriggerRunOnStart); err != nil {
		t.Fatal(err)
	}
	assertIntegrationCompleted(t, env)
	var payoutRequests []hyperliquidmock.RecordedRequest
	ledgerQueries := 0
	for _, request := range env.server.Requests() {
		if request.ActionType == "spotSend" && strings.EqualFold(request.Destination, integrationRecipient) {
			payoutRequests = append(payoutRequests, request)
		}
		if request.InfoType == "userNonFundingLedgerUpdates" {
			ledgerQueries++
		}
	}
	if len(payoutRequests) != 2 || ledgerQueries != 2 {
		t.Fatalf("payout requests = %d, ledger queries = %d; want replay after hidden ledger", len(payoutRequests), ledgerQueries)
	}
	if payoutRequests[0].Nonce != payoutRequests[1].Nonce || payoutRequests[0].BodyHash != payoutRequests[1].BodyHash {
		t.Fatalf("payout replay changed request: %#v", payoutRequests)
	}
}

func TestIntegrationRecoveryMySQLOutageDoesNotRepeatPayout(t *testing.T) {
	env := newIntegrationEnvironment(t)
	env.repository.completeFailures = 1
	err := env.orchestrator(env.store).RunNew(context.Background(), funding.TriggerRunOnStart)
	if !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("RunNew() error = %v, want transient MySQL outage", err)
	}
	current, loadErr := env.store.Load(context.Background())
	if loadErr != nil || current == nil || current.Phase != funding.PhaseDBUpdating {
		t.Fatalf("state during outage = %#v, %v", current, loadErr)
	}
	if got := recipientBalance(t, env); !got.Equal(env.payoutAmount) {
		t.Fatalf("recipient balance during outage = %s", got)
	}
	env.clock.advance(4 * time.Minute)
	if err := env.orchestrator(env.store).Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if env.repository.completeCalls != 2 {
		t.Fatalf("Complete() calls = %d, want one failure and one recovery", env.repository.completeCalls)
	}
	for call, ids := range env.repository.completeIDs {
		if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
			t.Fatalf("Complete() ids on call %d = %v", call+1, ids)
		}
	}
	assertIntegrationCompleted(t, env)
}

func TestIntegrationRecoveryUsesValidBackupWhenPrimaryIsCorrupt(t *testing.T) {
	env := newIntegrationEnvironment(t)
	crashing := &crashStore{StateStore: env.store, match: func(saved funding.RunState) bool {
		return saved.Phase == funding.PhasePayoutAccepted
	}}
	if err := env.orchestrator(crashing).RunNew(context.Background(), funding.TriggerRunOnStart); !errors.Is(err, errSimulatedCrash) {
		t.Fatalf("RunNew() error = %v, want simulated crash", err)
	}
	if err := os.WriteFile(filepath.Join(env.dataDir, "current.json"), []byte("corrupt primary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := env.orchestrator(env.store).Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertIntegrationCompleted(t, env)
}

type integrationEnvironment struct {
	server       *hyperliquidmock.Server
	repository   *integrationRepository
	store        *state.Store
	clock        *integrationClock
	nonce        *integrationNonce
	chain        integrationChain
	builders     []funding.Builder
	settlement   string
	dataDir      string
	payoutAmount decimal.Decimal
}

func newIntegrationEnvironment(t *testing.T, options ...hyperliquidmock.Option) *integrationEnvironment {
	t.Helper()
	server := hyperliquidmock.New(t, options...)
	keys := []string{integrationKey1, integrationKey2, integrationKey3}
	signers := make(map[string]signing.PrivateKey, len(keys))
	addresses := make([]string, len(keys))
	for index, value := range keys {
		key, err := signing.ParsePrivateKey(secret.NewString(value))
		if err != nil {
			t.Fatal(err)
		}
		address, err := key.Address()
		if err != nil {
			t.Fatal(err)
		}
		signers[address] = key
		addresses[index] = address
	}
	transport, err := httpclient.New(httpclient.Config{
		Network: hyperliquid.NetworkTestnet, BaseURL: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	exchangeClient, err := exchange.New(transport, hyperliquid.NetworkTestnet, signers)
	if err != nil {
		t.Fatal(err)
	}
	if transport.BaseURL() != server.URL {
		t.Fatalf("base URL override = %q, want %q", transport.BaseURL(), server.URL)
	}
	server.SetSpotBalance(addresses[0], "USDC:0", "0.5")
	server.SetClaimReward(addresses[0], "USDC:0", "1")
	server.SetSpotBalance(addresses[1], "USDC:0", "1")
	server.SetClaimReward(addresses[1], "USDC:0", "1")
	server.SetSpotBalance(addresses[2], "USDC:0", "0")
	server.SetSpotBalance(integrationRecipient, "USDC:0", "0")
	clock := &integrationClock{now: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)}
	repository := &integrationRepository{records: []funding.Record{
		{ID: 2, PeriodStartAt: 200, Amount: "2.000000000000000001"},
		{ID: 1, PeriodStartAt: 100, Amount: "1.123456000000000000"},
	}}
	dataDir := filepath.Join(t.TempDir(), "data")
	return &integrationEnvironment{
		server: server, repository: repository, store: state.NewStore(dataDir),
		clock:      clock,
		nonce:      &integrationNonce{next: uint64(clock.Now().UnixMilli() - 1)},
		chain:      integrationChain{info: info.New(transport), exchange: exchangeClient},
		builders:   []funding.Builder{{Name: "builder-1", Address: addresses[0]}, {Name: "builder-2", Address: addresses[1]}},
		settlement: addresses[2], dataDir: dataDir, payoutAmount: decimal.RequireFromString("3.123457"),
	}
}

func (e *integrationEnvironment) orchestrator(store funding.StateStore) *funding.Orchestrator {
	orchestrator, err := funding.NewOrchestrator(funding.OrchestratorConfig{
		Repository: e.repository, Store: store, Chain: e.chain,
		Builders: e.builders, Settlement: e.settlement, Recipient: integrationRecipient,
		Clock: e.clock, Nonce: e.nonce, Sleeper: integrationSleeper{clock: e.clock},
	})
	if err != nil {
		panic(err)
	}
	return orchestrator
}

type integrationSleeper struct{ clock *integrationClock }

func (s integrationSleeper) Sleep(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.clock.advance(delay)
	return nil
}

func assertIntegrationCompleted(t *testing.T, env *integrationEnvironment) {
	t.Helper()
	if !env.repository.completed {
		t.Fatal("repository was not completed")
	}
	current, err := env.store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if current != nil {
		t.Fatalf("current state still exists: phase=%s", current.Phase)
	}
	entries, err := os.ReadDir(filepath.Join(env.dataDir, "history"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("history entries = %d, error = %v", len(entries), err)
	}
	balance := recipientBalance(t, env)
	if !balance.Equal(env.payoutAmount) {
		t.Fatalf("recipient balance = %s, want exactly one payout of %s", balance, env.payoutAmount)
	}
}

func recipientBalance(t *testing.T, env *integrationEnvironment) decimal.Decimal {
	return integrationSpotBalance(t, env, integrationRecipient)
}

func integrationSpotBalance(t *testing.T, env *integrationEnvironment, address string) decimal.Decimal {
	t.Helper()
	transport, err := httpclient.New(httpclient.Config{Network: hyperliquid.NetworkTestnet, BaseURL: env.server.URL})
	if err != nil {
		t.Fatal(err)
	}
	token := info.Token{Name: "USDC", TokenID: "0", Index: 0, WeiDecimals: 6, WireToken: "USDC:0"}
	balance, err := info.New(transport).AvailableSpotBalance(context.Background(), address, token)
	if err != nil {
		t.Fatal(err)
	}
	return balance
}

type integrationChain struct {
	info     *info.Client
	exchange *exchange.Client
}

func (c integrationChain) CanonicalUSDC(ctx context.Context) (info.Token, error) {
	return c.info.CanonicalUSDC(ctx)
}

func (c integrationChain) AvailableSpotBalance(ctx context.Context, address string, token info.Token) (decimal.Decimal, error) {
	return c.info.AvailableSpotBalance(ctx, address, token)
}

func (c integrationChain) PrepareClaim(address string, nonce uint64) (exchange.PreparedAction, error) {
	return c.exchange.PrepareClaim(address, nonce)
}

func (c integrationChain) PrepareSpotSend(address, destination string, token info.Token, amount decimal.Decimal, nonce uint64) (exchange.PreparedAction, error) {
	return c.exchange.PrepareSpotSend(address, destination, token, amount, nonce)
}

func (c integrationChain) Submit(ctx context.Context, action exchange.PreparedAction) (exchange.SubmitResult, error) {
	return c.exchange.Submit(ctx, action)
}

func (c integrationChain) FindSpotTransfer(ctx context.Context, query info.TransferQuery) (*info.LedgerUpdate, bool, error) {
	updates, err := c.info.NonFundingLedger(ctx, query.Sender, query.StartTime, query.EndTime)
	if err != nil {
		return nil, false, err
	}
	update, matched := info.MatchSpotTransfer(updates, query)
	return update, matched, nil
}

type integrationRepository struct {
	mu               sync.Mutex
	records          []funding.Record
	completed        bool
	completeCalls    int
	completeFailures int
	completeIDs      [][]uint64
}

func (r *integrationRepository) ListPending(context.Context) ([]funding.Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]funding.Record(nil), r.records...), nil
}

func (r *integrationRepository) Complete(ctx context.Context, ids []uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.completeCalls++
	r.completeIDs = append(r.completeIDs, append([]uint64(nil), ids...))
	if r.completeFailures > 0 {
		r.completeFailures--
		return driver.ErrBadConn
	}
	r.completed = true
	return nil
}

type integrationClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *integrationClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *integrationClock) advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

type integrationNonce struct {
	mu   sync.Mutex
	next uint64
}

func (n *integrationNonce) Next() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.next++
	return n.next
}

var errSimulatedCrash = errors.New("simulated process crash")

type crashStore struct {
	funding.StateStore
	match   func(funding.RunState) bool
	crashed bool
}

func (s *crashStore) Save(ctx context.Context, saved funding.RunState) error {
	if err := s.StateStore.Save(ctx, saved); err != nil {
		return err
	}
	if !s.crashed && s.match(saved) {
		s.crashed = true
		return errSimulatedCrash
	}
	return nil
}

type callbackStore struct {
	funding.StateStore
	after func(funding.RunState)
}

func (s *callbackStore) Save(ctx context.Context, saved funding.RunState) error {
	if err := s.StateStore.Save(ctx, saved); err != nil {
		return err
	}
	if s.after != nil {
		s.after(saved)
	}
	return nil
}
