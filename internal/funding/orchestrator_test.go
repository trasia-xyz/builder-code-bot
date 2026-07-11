package funding

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"hyperliquid-builder-code-bot/internal/hyperliquid/exchange"
	"hyperliquid-builder-code-bot/internal/hyperliquid/info"
)

func TestNewOrchestratorRejectsSettlementInBuilderList(t *testing.T) {
	fx := newOrchestratorFixture(t)
	_, err := NewOrchestrator(OrchestratorConfig{
		Repository: fx.repo, Store: fx.store, Chain: fx.chain,
		Builders:   []Builder{{Name: "builder", Address: "0xSETTLEMENT"}},
		Settlement: "0xsettlement", Recipient: "0xrecipient", Clock: fixedClock{}, Nonce: &fakeNonce{},
	})
	if err == nil {
		t.Fatal("NewOrchestrator() error = nil")
	}
}

func TestTransferQueryBoundsLedgerByActionTimeAndCurrentTime(t *testing.T) {
	fx := newOrchestratorFixture(t)
	state := positiveState(t, PhasePayoutSubmitting)
	action := prepared("spotSend", "0xsettlement", "0xrecipient", "1.25", 10_000)

	query, err := fx.orchestrator.transferQuery(&state, action)
	if err != nil {
		t.Fatal(err)
	}
	wantStart := uint64(5_000)
	wantEnd := uint64(fx.orchestrator.clock.Now().UnixMilli() + ledgerFutureMargin.Milliseconds())
	if query.ActionTime != 10_000 || query.StartTime != wantStart || query.EndTime != wantEnd {
		t.Fatalf("query times = action:%d start:%d end:%d, want action:10000 start:%d end:%d", query.ActionTime, query.StartTime, query.EndTime, wantStart, wantEnd)
	}
}

func TestRunNewClaimsEveryBuilderBeforeSweepingAllAvailableUSDCAndPaysOnce(t *testing.T) {
	fx := newOrchestratorFixture(t)
	fx.repo.records = []Record{{ID: 9, Amount: "1.000000000000000001"}}
	fx.chain.balances = map[string]decimal.Decimal{
		"0xbuilder1":   decimal.RequireFromString("1.25"),
		"0xbuilder2":   decimal.RequireFromString("0.75"),
		"0xsettlement": decimal.RequireFromString("10"),
	}
	fx.chain.submitResults = []submitOutcome{
		{err: errors.New("connection reset")},
		{result: exchange.SubmitResult{Accepted: true, Response: json.RawMessage(`{"status":"ok","private_key":"leak","nested":{"privateKey":"camel-leak","accessToken":"token-leak"}}`)}},
		{result: exchange.SubmitResult{Accepted: true}},
		{result: exchange.SubmitResult{Accepted: true}},
		{result: exchange.SubmitResult{Accepted: true}},
	}

	if err := fx.orchestrator.RunNew(context.Background(), TriggerUTC); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"canonical", "prepare_claim:0xbuilder1", "submit:claimRewards:0xbuilder1",
		"prepare_claim:0xbuilder2", "submit:claimRewards:0xbuilder2",
		"balance:0xbuilder1", "prepare_send:0xbuilder1:0xsettlement:1.25", "submit:spotSend:0xbuilder1",
		"balance:0xbuilder2", "prepare_send:0xbuilder2:0xsettlement:0.75", "submit:spotSend:0xbuilder2",
		"balance:0xsettlement", "prepare_send:0xsettlement:0xrecipient:1.000001", "submit:spotSend:0xsettlement",
	}
	if !reflect.DeepEqual(fx.chain.events, want) {
		t.Fatalf("chain events:\n got %v\nwant %v", fx.chain.events, want)
	}
	if fx.repo.completeCalls != 1 || !reflect.DeepEqual(fx.repo.completedIDs, []uint64{9}) {
		t.Fatalf("complete calls = %d, IDs = %v", fx.repo.completeCalls, fx.repo.completedIDs)
	}
	if strings.Contains(string(mustJSON(t, fx.store.saved)), "leak") {
		t.Fatal("persisted state contains unsanitized private key")
	}
	for _, snapshot := range fx.store.saved {
		for _, builder := range snapshot.Builders {
			for _, action := range []*ActionProgress{&builder.Claim, &builder.Sweep} {
				if action.Phase == ActionSubmitting && (action.Prepared == nil || len(action.Prepared.RequestBody) == 0) {
					t.Fatalf("submitting action lacks exact persisted request: %#v", action)
				}
			}
		}
	}
}

func TestRecoverRejectsTamperedPreparedBuilderSweepBeforeSubmit(t *testing.T) {
	fx := newOrchestratorFixture(t)
	state := positiveState(t, PhaseConsolidating)
	action := prepared("spotSend", "0xbuilder1", "0xattacker", "1.25", 88)
	action.Token = "USDC:0"
	state.Builders = []BuilderProgress{{
		Name: "one", Address: "0xbuilder1", Claim: ActionProgress{Phase: ActionAccepted},
		Sweep: ActionProgress{Phase: ActionPrepared, Prepared: &action, BalanceBefore: "1.25"},
	}}
	fx.store.current = &state

	if err := fx.orchestrator.Recover(context.Background()); err == nil {
		t.Fatal("Recover() error = nil")
	}
	if slicesContainPrefix(fx.chain.events, "submit:") {
		t.Fatalf("submitted tampered builder request: %v", fx.chain.events)
	}
}

func TestRunNewReconcilesAmbiguousBuilderSweepWithExactLedgerAndBalance(t *testing.T) {
	fx := newOrchestratorFixture(t)
	fx.repo.records = []Record{{ID: 10, Amount: "1"}}
	fx.chain.balances = map[string]decimal.Decimal{"0xbuilder2": decimal.Zero, "0xsettlement": decimal.RequireFromString("2")}
	fx.chain.balanceQueues = map[string][]decimal.Decimal{"0xbuilder1": {decimal.NewFromInt(1), decimal.Zero}}
	fx.chain.transferMatched = true
	fx.chain.transfer = &info.LedgerUpdate{Time: 103, Delta: info.SpotTransferDelta{
		Type: "spotTransfer", Token: "USDC", Amount: "1", User: "0xbuilder1", Destination: "0xsettlement",
	}}
	fx.chain.submitResults = []submitOutcome{
		{result: exchange.SubmitResult{Accepted: true}},
		{result: exchange.SubmitResult{Accepted: true}},
		{err: errors.New("timeout")},
		{result: exchange.SubmitResult{Accepted: true}},
	}

	if err := fx.orchestrator.RunNew(context.Background(), TriggerUTC); err != nil {
		t.Fatal(err)
	}
	if !slicesContain(fx.chain.events, "find:0xbuilder1:0xsettlement:103") {
		t.Fatalf("events = %v", fx.chain.events)
	}
	foundAccepted := false
	for _, snapshot := range fx.store.saved {
		if len(snapshot.Builders) != 0 && snapshot.Builders[0].Sweep.Phase == ActionAccepted && snapshot.Builders[0].Sweep.Evidence != nil {
			foundAccepted = true
		}
	}
	if !foundAccepted {
		t.Fatal("ambiguous builder sweep was not persisted as accepted with evidence")
	}
}

func TestRunNewNegativeArchivesValidationFailureWithoutCurrentOrDBUpdate(t *testing.T) {
	fx := newOrchestratorFixture(t)
	fx.repo.records = []Record{{ID: 7, Amount: "-0.000000000000000001"}}

	err := fx.orchestrator.RunNew(context.Background(), TriggerRunOnStart)
	if !errors.Is(err, ErrNegativeAmount) {
		t.Fatalf("RunNew() error = %v, want ErrNegativeAmount", err)
	}
	if fx.store.current != nil || fx.store.saveCalls != 0 || fx.repo.completeCalls != 0 {
		t.Fatalf("current = %#v, saves = %d, completes = %d", fx.store.current, fx.store.saveCalls, fx.repo.completeCalls)
	}
	if !reflect.DeepEqual(fx.store.archives, []string{"failed_validation"}) {
		t.Fatalf("archives = %v", fx.store.archives)
	}
	if fx.chain.calls != 0 {
		t.Fatalf("chain calls = %d", fx.chain.calls)
	}
}

func TestRunNewNoRecordsArchivesNoDataWithoutCurrentChainOrDBMutation(t *testing.T) {
	fx := newOrchestratorFixture(t)
	if err := fx.orchestrator.RunNew(context.Background(), TriggerUTC); err != nil {
		t.Fatal(err)
	}
	if fx.store.current != nil || fx.store.saveCalls != 0 || fx.chain.calls != 0 || fx.repo.completeCalls != 0 {
		t.Fatalf("current = %#v, saves = %d, chain = %d, completes = %d", fx.store.current, fx.store.saveCalls, fx.chain.calls, fx.repo.completeCalls)
	}
	if !reflect.DeepEqual(fx.store.archives, []string{"no_data"}) {
		t.Fatalf("archives = %v", fx.store.archives)
	}
}

func TestRunNewInsufficientSettlementBlocksWithoutPayoutOrDatabaseUpdate(t *testing.T) {
	fx := newOrchestratorFixture(t)
	fx.repo.records = []Record{{ID: 8, Amount: "2"}}
	fx.chain.balances = map[string]decimal.Decimal{}
	if err := fx.orchestrator.RunNew(context.Background(), TriggerUTC); err == nil {
		t.Fatal("RunNew() error = nil")
	}
	if fx.repo.completeCalls != 0 || fx.store.current == nil || fx.store.current.Phase != PhaseBlocked {
		t.Fatalf("completes = %d, current = %#v", fx.repo.completeCalls, fx.store.current)
	}
	if slicesContainPrefix(fx.chain.events, "prepare_send:0xsettlement") {
		t.Fatalf("partial payout prepared: %v", fx.chain.events)
	}
}

func TestRunNewZeroTotalPersistsRecoverableStateSkipsChainAndCompletesManifestIDs(t *testing.T) {
	fx := newOrchestratorFixture(t)
	fx.repo.records = []Record{{ID: 3, PeriodStartAt: 2, Amount: "0"}, {ID: 2, PeriodStartAt: 1, Amount: "0.000000000000000000"}}

	if err := fx.orchestrator.RunNew(context.Background(), TriggerUTC); err != nil {
		t.Fatal(err)
	}
	if fx.chain.calls != 0 {
		t.Fatalf("chain calls = %d", fx.chain.calls)
	}
	if !reflect.DeepEqual(fx.repo.completedIDs, []uint64{2, 3}) {
		t.Fatalf("completed IDs = %v", fx.repo.completedIDs)
	}
	if fx.store.saveCalls < 3 || fx.store.current != nil || !reflect.DeepEqual(fx.store.archives, []string{"completed"}) {
		t.Fatalf("saves = %d, current = %#v, archives = %v", fx.store.saveCalls, fx.store.current, fx.store.archives)
	}
	if fx.store.saved[0].Phase != PhasePrepared || fx.store.saved[1].Phase != PhaseDBUpdating {
		t.Fatalf("saved phases = %v, %v", fx.store.saved[0].Phase, fx.store.saved[1].Phase)
	}
	if fx.store.saved[0].Manifest.Token != nil {
		t.Fatalf("zero manifest token = %#v", fx.store.saved[0].Manifest.Token)
	}
}

func TestRecoverAcceptedPayoutOnlyCompletesDatabase(t *testing.T) {
	fx := newOrchestratorFixture(t)
	state := positiveState(t, PhasePayoutAccepted)
	state.FinalPayout = &ActionProgress{Phase: ActionAccepted, Prepared: ptrPrepared(prepared("spotSend", "0xsettlement", "0xrecipient", "1.25", 77))}
	fx.store.current = &state

	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fx.chain.calls != 0 || fx.repo.completeCalls != 1 || !reflect.DeepEqual(fx.repo.completedIDs, []uint64{41}) {
		t.Fatalf("chain calls = %d, complete calls = %d, IDs = %v", fx.chain.calls, fx.repo.completeCalls, fx.repo.completedIDs)
	}
}

func TestRecoverDBUpdatingRetriesOnlyManifestCompletion(t *testing.T) {
	fx := newOrchestratorFixture(t)
	state := positiveState(t, PhaseDBUpdating)
	fx.store.current = &state
	fx.repo.completeErr = errors.New("mysql unavailable")
	if err := fx.orchestrator.Recover(context.Background()); err == nil {
		t.Fatal("first Recover() error = nil")
	}
	fx.repo.completeErr = nil
	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fx.chain.calls != 0 || fx.repo.completeCalls != 2 {
		t.Fatalf("chain calls = %d, complete calls = %d", fx.chain.calls, fx.repo.completeCalls)
	}
}

func TestRecoverUnknownPayoutConfirmsLedgerWithoutNonceWhenBalanceDeltaMatches(t *testing.T) {
	fx := newOrchestratorFixture(t)
	state := positiveState(t, PhasePayoutSubmitting)
	action := prepared("spotSend", "0xsettlement", "0xrecipient", "1.25", 77)
	action.Token = "USDC:0"
	state.FinalPayout = &ActionProgress{Phase: ActionUnknown, Prepared: &action, SubmitAttempts: 2, BalanceBefore: "5"}
	fx.store.current = &state
	fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.RequireFromString("3.75")}
	fx.chain.transferMatched = true
	fx.chain.transfer = &info.LedgerUpdate{Time: 77, Delta: info.SpotTransferDelta{
		Type: "spotTransfer", Token: "USDC", Amount: "1.25", User: "0xsettlement", Destination: "0xrecipient",
	}}

	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fx.repo.completeCalls != 1 || fx.chain.submitIndex != 0 {
		t.Fatalf("completes = %d, submits = %d", fx.repo.completeCalls, fx.chain.submitIndex)
	}
}

func TestRecoverUnknownPayoutConfirmsExactLedgerAndBalanceWithoutResubmit(t *testing.T) {
	fx := newOrchestratorFixture(t)
	state := positiveState(t, PhasePayoutSubmitting)
	action := prepared("spotSend", "0xsettlement", "0xrecipient", "1.25", 77)
	action.Token = "USDC:0"
	state.FinalPayout = &ActionProgress{Phase: ActionUnknown, Prepared: &action, SubmitAttempts: 1, BalanceBefore: "5"}
	fx.store.current = &state
	fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.RequireFromString("3.75")}
	fx.chain.transferMatched = true
	fx.chain.transfer = &info.LedgerUpdate{Time: 77, Delta: info.SpotTransferDelta{
		Type: "spotTransfer", Token: "USDC", Amount: "1.25", User: "0xsettlement", Destination: "0xrecipient",
	}}

	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fx.chain.submitIndex != 0 || fx.repo.completeCalls != 1 {
		t.Fatalf("submits = %d, completes = %d", fx.chain.submitIndex, fx.repo.completeCalls)
	}
}

func TestRecoverUnknownPayoutResubmitsOnlyPersistedExactRequest(t *testing.T) {
	fx := newOrchestratorFixture(t)
	state := positiveState(t, PhasePayoutSubmitting)
	action := prepared("spotSend", "0xsettlement", "0xrecipient", "1.25", 77)
	action.Token = "USDC:0"
	state.FinalPayout = &ActionProgress{Phase: ActionUnknown, Prepared: &action, SubmitAttempts: 1, BalanceBefore: "5"}
	fx.store.current = &state
	fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.RequireFromString("5")}
	fx.chain.submitResults = []submitOutcome{{result: exchange.SubmitResult{Accepted: true}}}

	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fx.chain.events, []string{"find:0xsettlement:0xrecipient:77", "balance:0xsettlement", "submit:spotSend:0xsettlement"}) {
		t.Fatalf("events = %v", fx.chain.events)
	}
	for _, event := range fx.chain.events {
		if strings.HasPrefix(event, "prepare_") {
			t.Fatalf("recovery created a replacement action: %v", fx.chain.events)
		}
	}
}

func TestRecoverReconcilesAgainAfterAmbiguousExactResubmit(t *testing.T) {
	fx := newOrchestratorFixture(t)
	state := positiveState(t, PhasePayoutSubmitting)
	action := prepared("spotSend", "0xsettlement", "0xrecipient", "1.25", 77)
	action.Token = "USDC:0"
	state.FinalPayout = &ActionProgress{Phase: ActionUnknown, Prepared: &action, SubmitAttempts: 1, BalanceBefore: "5"}
	fx.store.current = &state
	fx.chain.balanceQueues = map[string][]decimal.Decimal{"0xsettlement": {decimal.NewFromInt(5), decimal.RequireFromString("3.75")}}
	evidence := &info.LedgerUpdate{Time: 77, Delta: info.SpotTransferDelta{Type: "spotTransfer", Token: "USDC", Amount: "1.25", User: "0xsettlement", Destination: "0xrecipient"}}
	fx.chain.transferOutcomes = []transferOutcome{{}, {transfer: evidence, matched: true}}
	fx.chain.submitResults = []submitOutcome{{err: errors.New("timeout")}}

	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fx.repo.completeCalls != 1 || countPrefix(fx.chain.events, "submit:") != 1 || countPrefix(fx.chain.events, "find:") != 2 {
		t.Fatalf("events = %v, completes = %d", fx.chain.events, fx.repo.completeCalls)
	}
}

func TestRecoverReconcilesAgainWhenExactResubmitIsRejected(t *testing.T) {
	fx := newOrchestratorFixture(t)
	state := positiveState(t, PhasePayoutSubmitting)
	action := prepared("spotSend", "0xsettlement", "0xrecipient", "1.25", 77)
	action.Token = "USDC:0"
	state.FinalPayout = &ActionProgress{Phase: ActionUnknown, Prepared: &action, SubmitAttempts: 1, BalanceBefore: "5"}
	fx.store.current = &state
	fx.chain.balanceQueues = map[string][]decimal.Decimal{"0xsettlement": {decimal.NewFromInt(5), decimal.RequireFromString("3.75")}}
	evidence := &info.LedgerUpdate{Time: 77, Delta: info.SpotTransferDelta{Type: "spotTransfer", Token: "USDC", Amount: "1.25", User: "0xsettlement", Destination: "0xrecipient"}}
	fx.chain.transferOutcomes = []transferOutcome{{}, {transfer: evidence, matched: true}}
	fx.chain.submitResults = []submitOutcome{{result: exchange.SubmitResult{Rejected: true}}}

	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fx.repo.completeCalls != 1 || countPrefix(fx.chain.events, "find:") != 2 {
		t.Fatalf("events = %v, completes = %d", fx.chain.events, fx.repo.completeCalls)
	}
}

func TestRecoverPreparedPayoutSubmitsPersistedRequestWithoutPreparingReplacement(t *testing.T) {
	fx := newOrchestratorFixture(t)
	state := positiveState(t, PhaseFunded)
	action := prepared("spotSend", "0xsettlement", "0xrecipient", "1.25", 77)
	action.Token = "USDC:0"
	state.FinalPayout = &ActionProgress{Phase: ActionPrepared, Prepared: &action, BalanceBefore: "5"}
	fx.store.current = &state
	fx.chain.submitResults = []submitOutcome{{result: exchange.SubmitResult{Accepted: true}}}

	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fx.chain.events, []string{"submit:spotSend:0xsettlement"}) {
		t.Fatalf("events = %v", fx.chain.events)
	}
}

func TestRecoverRejectsPreparedPayoutThatDoesNotMatchImmutableManifest(t *testing.T) {
	fx := newOrchestratorFixture(t)
	state := positiveState(t, PhaseFunded)
	action := prepared("spotSend", "0xsettlement", "0xattacker", "1.25", 77)
	action.Token = "USDC:0"
	state.FinalPayout = &ActionProgress{Phase: ActionPrepared, Prepared: &action, BalanceBefore: "5"}
	fx.store.current = &state

	if err := fx.orchestrator.Recover(context.Background()); err == nil {
		t.Fatal("Recover() error = nil")
	}
	if slicesContainPrefix(fx.chain.events, "submit:") {
		t.Fatalf("submitted action that diverges from manifest: %v", fx.chain.events)
	}
}

func TestRunNewDoesNotMutateChainWhenSubmittingSnapshotSaveFailsAndRecoverResumes(t *testing.T) {
	fx := newOrchestratorFixture(t)
	fx.repo.records = []Record{{ID: 51, Amount: "1"}}
	fx.chain.balances = map[string]decimal.Decimal{"0xsettlement": decimal.RequireFromString("2")}
	fx.store.failSaveAt = 4

	if err := fx.orchestrator.RunNew(context.Background(), TriggerUTC); err == nil {
		t.Fatal("RunNew() error = nil")
	}
	for _, event := range fx.chain.events {
		if strings.HasPrefix(event, "submit:") {
			t.Fatalf("chain mutation occurred after failed pre-submit save: %v", fx.chain.events)
		}
	}
	fx.store.failSaveAt = 0
	if err := fx.orchestrator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fx.repo.completeCalls != 1 {
		t.Fatalf("complete calls = %d", fx.repo.completeCalls)
	}
}

type orchestratorFixture struct {
	orchestrator *Orchestrator
	repo         *fakeRepository
	store        *fakeStateStore
	chain        *fakeChain
	notifier     *fakeNotifier
	events       *[]string
}

func newOrchestratorFixture(t *testing.T) *orchestratorFixture {
	return newOrchestratorFixtureWithLogger(t, nil)
}

func newOrchestratorFixtureWithLogger(t *testing.T, logger Logger) *orchestratorFixture {
	t.Helper()
	repo := &fakeRepository{}
	store := &fakeStateStore{}
	chain := &fakeChain{token: info.Token{Name: "USDC", TokenID: "0", Index: 0, WeiDecimals: 6, WireToken: "USDC:0"}}
	notifier := &fakeNotifier{}
	events := &[]string{}
	repo.events = events
	store.events = events
	o, err := NewOrchestrator(OrchestratorConfig{
		Repository: repo,
		Store:      store,
		Chain:      chain,
		Notifier:   notifier,
		Logger:     logger,
		Builders:   []Builder{{Name: "one", Address: "0xbuilder1"}, {Name: "two", Address: "0xbuilder2"}},
		Settlement: "0xsettlement",
		Recipient:  "0xrecipient",
		Clock:      fixedClock{time.Date(2026, 7, 11, 1, 2, 3, 0, time.UTC)},
		Nonce:      &fakeNonce{next: 100},
		Sleeper:    &recordingSleeper{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &orchestratorFixture{orchestrator: o, repo: repo, store: store, chain: chain, notifier: notifier, events: events}
}

type fakeRepository struct {
	records       []Record
	completedIDs  []uint64
	completeCalls int
	listCalls     int
	completeErr   error
	events        *[]string
}

func (r *fakeRepository) ListPending(context.Context) ([]Record, error) {
	r.listCalls++
	appendEvent(r.events, "repo.list_pending")
	return append([]Record(nil), r.records...), nil
}
func (r *fakeRepository) Complete(_ context.Context, ids []uint64) error {
	r.completeCalls++
	appendEvent(r.events, "repo.complete")
	r.completedIDs = append([]uint64(nil), ids...)
	return r.completeErr
}

type fakeStateStore struct {
	current        *RunState
	saved          []RunState
	archives       []string
	archived       []RunState
	saveCalls      int
	loadCalls      int
	failSaveAt     int
	failSave       func(RunState) bool
	crashAfterSave func(RunState) bool
	archiveErr     error
	clearErr       error
	events         *[]string
	loadErr        error
	loadMeta       StateLoadMetadata
}

func (s *fakeStateStore) Load(context.Context) (*RunState, error) {
	s.loadCalls++
	return s.current, s.loadErr
}
func (s *fakeStateStore) LoadWithMetadata(context.Context) (*RunState, StateLoadMetadata, error) {
	s.loadCalls++
	return s.current, s.loadMeta, s.loadErr
}
func (s *fakeStateStore) Save(_ context.Context, state RunState) error {
	s.saveCalls++
	appendEvent(s.events, "store.save:"+string(state.Phase)+":"+actionSnapshot(state))
	if s.failSaveAt != 0 && s.saveCalls == s.failSaveAt || s.failSave != nil && s.failSave(state) {
		return errors.New("save failpoint")
	}
	copy := cloneRunState(state)
	s.saved = append(s.saved, copy)
	s.current = &copy
	if s.crashAfterSave != nil && s.crashAfterSave(state) {
		return errors.New("simulated process exit after durable save")
	}
	return nil
}
func (s *fakeStateStore) Archive(_ context.Context, state RunState, result string) error {
	appendEvent(s.events, "store.archive:"+result)
	s.archives = append(s.archives, result)
	s.archived = append(s.archived, cloneRunState(state))
	return s.archiveErr
}
func (s *fakeStateStore) Clear(context.Context) error {
	appendEvent(s.events, "store.clear")
	if s.clearErr != nil {
		return s.clearErr
	}
	s.current = nil
	return nil
}

type fakeChain struct {
	token            info.Token
	calls            int
	events           []string
	balances         map[string]decimal.Decimal
	balanceQueues    map[string][]decimal.Decimal
	submitResults    []submitOutcome
	submitIndex      int
	transfer         *info.LedgerUpdate
	transferMatched  bool
	transferOutcomes []transferOutcome
	prepareClaimErr  map[string]error
	prepareSendErr   map[string]error
	balanceErr       map[string]error
}

type transferOutcome struct {
	transfer *info.LedgerUpdate
	matched  bool
	err      error
}

type submitOutcome struct {
	result exchange.SubmitResult
	err    error
}

func (c *fakeChain) CanonicalUSDC(context.Context) (info.Token, error) {
	c.calls++
	c.events = append(c.events, "canonical")
	return c.token, nil
}
func (c *fakeChain) AvailableSpotBalance(_ context.Context, address string, _ info.Token) (decimal.Decimal, error) {
	c.calls++
	c.events = append(c.events, "balance:"+address)
	if err := c.balanceErr[address]; err != nil {
		return decimal.Zero, err
	}
	if queue := c.balanceQueues[address]; len(queue) != 0 {
		value := queue[0]
		c.balanceQueues[address] = queue[1:]
		return value, nil
	}
	return c.balances[address], nil
}
func (c *fakeChain) PrepareClaim(address string, nonce uint64) (exchange.PreparedAction, error) {
	c.calls++
	c.events = append(c.events, "prepare_claim:"+address)
	if err := c.prepareClaimErr[address]; err != nil {
		return exchange.PreparedAction{}, err
	}
	return prepared("claimRewards", address, "", "", nonce), nil
}
func (c *fakeChain) PrepareSpotSend(address, destination string, token info.Token, amount decimal.Decimal, nonce uint64) (exchange.PreparedAction, error) {
	c.calls++
	c.events = append(c.events, "prepare_send:"+address+":"+destination+":"+amount.String())
	if err := c.prepareSendErr[address]; err != nil {
		return exchange.PreparedAction{}, err
	}
	action := prepared("spotSend", address, destination, amount.String(), nonce)
	action.Token = token.WireToken
	return action, nil
}
func (c *fakeChain) Submit(_ context.Context, action exchange.PreparedAction) (exchange.SubmitResult, error) {
	c.calls++
	c.events = append(c.events, "submit:"+action.Kind+":"+action.Signer)
	if c.submitIndex >= len(c.submitResults) {
		return exchange.SubmitResult{Accepted: true}, nil
	}
	outcome := c.submitResults[c.submitIndex]
	c.submitIndex++
	return outcome.result, outcome.err
}
func (c *fakeChain) FindSpotTransfer(_ context.Context, query info.TransferQuery) (*info.LedgerUpdate, bool, error) {
	c.calls++
	c.events = append(c.events, fmt.Sprintf("find:%s:%s:%d", query.Sender, query.Destination, query.ActionTime))
	if len(c.transferOutcomes) != 0 {
		outcome := c.transferOutcomes[0]
		c.transferOutcomes = c.transferOutcomes[1:]
		return outcome.transfer, outcome.matched, outcome.err
	}
	return c.transfer, c.transferMatched, nil
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type fakeNonce struct{ next uint64 }

func (n *fakeNonce) Next() uint64 { n.next++; return n.next }

type fakeNotifier struct {
	keys     []string
	messages []string
}

func (n *fakeNotifier) Alert(_ context.Context, key, message string) error {
	n.keys = append(n.keys, key)
	n.messages = append(n.messages, message)
	return nil
}

func appendEvent(events *[]string, event string) {
	if events != nil {
		*events = append(*events, event)
	}
}

func actionSnapshot(state RunState) string {
	if state.FinalPayout != nil {
		return "payout=" + string(state.FinalPayout.Phase)
	}
	for _, builder := range state.Builders {
		if builder.Sweep.Phase != "" {
			return "sweep=" + string(builder.Sweep.Phase)
		}
		if builder.Claim.Phase != "" {
			return "claim=" + string(builder.Claim.Phase)
		}
	}
	return "none"
}

func prepared(kind, signer, destination, amount string, nonce uint64) exchange.PreparedAction {
	var body json.RawMessage
	if kind == "spotSend" {
		body = json.RawMessage(fmt.Sprintf(`{"action":{"type":"spotSend","destination":%q,"token":"USDC:0","amount":%q,"time":%d},"nonce":%d}`, destination, amount, nonce, nonce))
	} else {
		body = json.RawMessage(fmt.Sprintf(`{"action":{"type":"claimRewards"},"nonce":%d}`, nonce))
	}
	digest := sha256.Sum256(body)
	return exchange.PreparedAction{Kind: kind, Signer: signer, Destination: destination, Amount: amount, Nonce: nonce, RequestHash: fmt.Sprintf("%x", digest), RequestBody: body}
}

func cloneRunState(state RunState) RunState {
	data, _ := json.Marshal(state)
	var copy RunState
	_ = json.Unmarshal(data, &copy)
	return copy
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func positiveState(t *testing.T, phase Phase) RunState {
	t.Helper()
	token := info.Token{Name: "USDC", TokenID: "0", Index: 0, WeiDecimals: 6, WireToken: "USDC:0"}
	manifest, err := BuildManifest(ManifestInput{
		Records: []Record{{ID: 41, Amount: "1.25"}}, Token: &token,
		Builders: []string{"0xbuilder1", "0xbuilder2"}, Settlement: "0xsettlement", Recipient: "0xrecipient",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 11, 1, 2, 3, 0, time.UTC)
	return RunState{RunID: "run", Trigger: TriggerUTC, UTCDate: "2026-07-11", Phase: phase, Manifest: manifest, CreatedAt: now, UpdatedAt: now}
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func slicesContainPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func countPrefix(values []string, prefix string) int {
	count := 0
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			count++
		}
	}
	return count
}
