package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"builder-code-bot/internal/config"
	"builder-code-bot/internal/crypt/keycipher"
	"builder-code-bot/internal/dev/hyperliquidmock"
	"builder-code-bot/internal/funding"
	"builder-code-bot/internal/hyperliquid/signing"
	"builder-code-bot/internal/logging"
	"builder-code-bot/internal/scheduler"
	"builder-code-bot/internal/secret"
	"builder-code-bot/internal/state"
)

const testPrivateKey = "0x0000000000000000000000000000000000000000000000000000000000000001"

func TestResolvePasswordPrefersConfiguredValue(t *testing.T) {
	prompt := &fakeSecretPrompt{err: errors.New("must not be called")}
	got, err := resolvePassword(secret.NewString("configured"), prompt)
	if err != nil {
		t.Fatalf("resolvePassword() error = %v", err)
	}
	if got.Reveal() != "configured" {
		t.Fatal("resolvePassword() did not return configured password")
	}
	if prompt.calls != 0 {
		t.Fatalf("prompt calls = %d, want 0", prompt.calls)
	}
}

func TestResolvePasswordPromptsOnceWhenConfigurationIsEmpty(t *testing.T) {
	prompt := &fakeSecretPrompt{value: secret.NewString("prompted")}
	got, err := resolvePassword(secret.SecretString{}, prompt)
	if err != nil {
		t.Fatalf("resolvePassword() error = %v", err)
	}
	if got.Reveal() != "prompted" {
		t.Fatal("resolvePassword() did not return prompted password")
	}
	if prompt.calls != 1 {
		t.Fatalf("prompt calls = %d, want 1", prompt.calls)
	}
}

func TestResolvePasswordReturnsNonTerminalPromptError(t *testing.T) {
	prompt := &fakeSecretPrompt{err: errors.New("secret input must be entered from a terminal")}
	_, err := resolvePassword(secret.SecretString{}, prompt)
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("resolvePassword() error = %v, want terminal error", err)
	}
	if prompt.calls != 1 {
		t.Fatalf("prompt calls = %d, want 1", prompt.calls)
	}
}

func TestResolvePasswordRejectsEmptyPromptedPassword(t *testing.T) {
	_, err := resolvePassword(secret.SecretString{}, &fakeSecretPrompt{})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("resolvePassword() error = %v, want empty password error", err)
	}
}

func TestBuildSignersDecryptsEveryAccountWithSharedPassword(t *testing.T) {
	password := secret.NewString("shared password")
	builderEncrypted, err := keycipher.Encrypt(secret.NewString(testPrivateKey), password)
	if err != nil {
		t.Fatal(err)
	}
	settlementPrivateKey := secret.NewString("0x0000000000000000000000000000000000000000000000000000000000000002")
	settlementKey, err := signing.ParsePrivateKey(settlementPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	settlementAddress, err := settlementKey.Address()
	if err != nil {
		t.Fatal(err)
	}
	settlementEncrypted, err := keycipher.Encrypt(settlementPrivateKey, password)
	if err != nil {
		t.Fatal(err)
	}
	builderAddress := "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf"
	cfg := config.Config{
		Builders:   []config.BuilderConfig{{Name: "builder", Address: builderAddress, EncryptedPrivateKey: builderEncrypted}},
		Settlement: config.AccountConfig{Address: settlementAddress, EncryptedPrivateKey: settlementEncrypted},
	}

	signers, err := buildSigners(cfg, password)
	if err != nil {
		t.Fatalf("buildSigners() error = %v", err)
	}
	if len(signers) != 2 {
		t.Fatalf("signers = %d, want builder and settlement", len(signers))
	}
	for _, address := range []string{builderAddress, settlementAddress} {
		if _, ok := signers[address]; !ok {
			t.Fatalf("signers missing configured address %s", address)
		}
	}
}

func TestBuildSignersRejectsAnyDerivedAddressMismatch(t *testing.T) {
	password := secret.NewString("shared password")
	encrypted, err := keycipher.Encrypt(secret.NewString(testPrivateKey), password)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Builders: []config.BuilderConfig{{
			Name: "builder", Address: "0x0000000000000000000000000000000000000002", EncryptedPrivateKey: encrypted,
		}},
		Settlement: config.AccountConfig{
			Address: "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf", EncryptedPrivateKey: encrypted,
		},
	}

	_, err = buildSigners(cfg, password)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("buildSigners() error = %v, want address mismatch", err)
	}
}

func TestBuildBuilderSignersDoesNotDecryptSettlementKey(t *testing.T) {
	password := secret.NewString("shared password")
	builderEncrypted, err := keycipher.Encrypt(secret.NewString(testPrivateKey), password)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Builders: []config.BuilderConfig{{
			Name: "builder", Address: "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf",
			EncryptedPrivateKey: builderEncrypted,
		}},
		Settlement: config.AccountConfig{
			Address:             "0x0000000000000000000000000000000000000002",
			EncryptedPrivateKey: secret.NewString("not valid ciphertext"),
		},
	}

	signers, err := buildBuilderSigners(cfg, password)
	if err != nil {
		t.Fatalf("buildBuilderSigners() error = %v", err)
	}
	if len(signers) != 1 {
		t.Fatalf("signers = %d, want one builder signer", len(signers))
	}
	if _, exists := signers[cfg.Settlement.Address]; exists {
		t.Fatal("builder signer set unexpectedly contains settlement")
	}
}

func TestAssembleOrchestratorWiresBothRepositoriesAndRecipients(t *testing.T) {
	server := hyperliquidmock.New(t)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mock.MatchExpectationsInOrder(false)
	emptyRows := func() *sqlmock.Rows {
		return sqlmock.NewRows([]string{"id", "period_start_at", "amount"})
	}
	mock.ExpectQuery("FROM trasia_points_rebate_funding_record").WillReturnRows(emptyRows())
	mock.ExpectQuery("FROM trasia_referral_funding_record").WillReturnRows(emptyRows())

	const (
		builderAddress        = "0x0000000000000000000000000000000000000001"
		settlementAddress     = "0x0000000000000000000000000000000000000002"
		pointsRebateRecipient = "0x0000000000000000000000000000000000000003"
		referralRecipient     = "0x0000000000000000000000000000000000000004"
	)
	cfg := config.Config{
		Hyperliquid: config.HyperliquidConfig{Network: "testnet", BaseURL: server.URL},
		Builders:    []config.BuilderConfig{{Name: "builder", Address: builderAddress}},
		Settlement:  config.AccountConfig{Address: settlementAddress},
		PointsRebatePayout: config.PayoutConfig{
			RecipientAddress: pointsRebateRecipient,
		},
		ReferralPayout: config.PayoutConfig{RecipientAddress: referralRecipient},
	}
	notifier := &recordingFundingNotifier{}
	orchestrator, err := assembleOrchestrator(
		cfg,
		map[string]signing.PrivateKey{},
		db,
		nil,
		notifier,
		logging.New(logging.Config{Format: "json", Output: io.Discard}),
	)
	if err != nil {
		t.Fatalf("assembleOrchestrator() error = %v", err)
	}
	if err := orchestrator.Run(context.Background(), funding.TriggerRunOnStart); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("repository queries were not both executed: %v", err)
	}
	if len(notifier.reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(notifier.reports))
	}

	report := notifier.reports[0]
	wantRecipients := map[funding.PayoutKind]string{
		funding.PayoutKindPointsRebate: pointsRebateRecipient,
		funding.PayoutKindReferral:     referralRecipient,
	}
	if len(report.Payouts) != len(wantRecipients) {
		t.Fatalf("payout reports = %d, want %d", len(report.Payouts), len(wantRecipients))
	}
	for _, payout := range report.Payouts {
		if payout.Payout.Recipient != wantRecipients[payout.Kind] {
			t.Errorf("%s recipient = %q, want %q", payout.Kind, payout.Payout.Recipient, wantRecipients[payout.Kind])
		}
		if !payout.RecordsRead || payout.RecordCount != 0 {
			t.Errorf("%s record snapshot = read:%v count:%d, want read empty snapshot", payout.Kind, payout.RecordsRead, payout.RecordCount)
		}
	}
}

func TestRuntimeRunOnStartRunsPayoutPhase(t *testing.T) {
	runner := &recordingRunner{}
	if err := startRuntime(context.Background(), runner, true); err != nil {
		t.Fatalf("startRuntime() error = %v", err)
	}
	want := []string{"run:run_on_start"}
	if strings.Join(runner.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", runner.calls, want)
	}
}

func TestRuntimeWithoutRunOnStartOnlyRecovers(t *testing.T) {
	runner := &recordingRunner{}
	if err := startRuntime(context.Background(), runner, false); err != nil {
		t.Fatalf("startRuntime() error = %v", err)
	}
	if got := strings.Join(runner.calls, ","); got != "recover" {
		t.Fatalf("calls = %v, want recover only", runner.calls)
	}
}

func TestAppRunUsesCallerContextAndReturnsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	scheduler := &recordingScheduler{}
	app := &App{orchestrator: &funding.Orchestrator{}, scheduler: scheduler}
	err := app.Run(ctx)
	if !errors.Is(err, context.Canceled) || scheduler.ctx != ctx {
		t.Fatalf("Run() = %v, context forwarded = %v", err, scheduler.ctx == ctx)
	}
}

func TestAppCloseReleasesDatabaseAndProcessLock(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectClose()
	dir := t.TempDir()
	lock, err := state.AcquireProcessLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{db: db, processLock: lock}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	reacquired, err := state.AcquireProcessLock(dir)
	if err != nil {
		t.Fatalf("process lock was not released: %v", err)
	}
	_ = reacquired.Close()
}

func TestLogScheduledFailureKeepsSafeSpecificCause(t *testing.T) {
	var output bytes.Buffer
	logger := logging.New(logging.Config{
		Format: logging.FormatJSON,
		Level:  logging.LevelDebug,
		Output: &output,
	})

	logScheduledFailure(logger, scheduler.TaskConsolidation,
		errors.New("claimable query: password=do-not-log; upstream timeout"))
	logScheduledFailure(logger, scheduler.TaskFunding,
		errors.New("store payout: deadlock detected"))

	logs := output.String()
	if strings.Contains(logs, "do-not-log") {
		t.Fatalf("scheduled failure log leaked credential: %s", logs)
	}
	for _, want := range []string{
		"claimable query", "upstream timeout", "store payout", "deadlock detected",
		`"event":"scheduled_daily_consolidation_failed"`,
		`"event":"scheduled_funding_run_failed"`,
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("scheduled failure log missing %q: %s", want, logs)
		}
	}
}

func TestJoinStartupCleanupErrorPreservesBothCauses(t *testing.T) {
	startupErr := errors.New("startup failed")
	cleanupErr := errors.New("close failed")
	got := joinStartupCleanupError(startupErr, "close dependency", cleanupErr)
	if !errors.Is(got, startupErr) || !errors.Is(got, cleanupErr) ||
		!strings.Contains(got.Error(), "close dependency") {
		t.Fatalf("joinStartupCleanupError() = %v", got)
	}
}

type fakeSecretPrompt struct {
	value secret.SecretString
	err   error
	calls int
}

func (p *fakeSecretPrompt) ReadSecret(string) (secret.SecretString, error) {
	p.calls++
	return p.value, p.err
}

type recordingRunner struct{ calls []string }

type recordingFundingNotifier struct{ reports []funding.RunReport }

func (*recordingFundingNotifier) Alert(context.Context, string, funding.AlertSeverity, string) {}

func (n *recordingFundingNotifier) Report(_ context.Context, report funding.RunReport) {
	n.reports = append(n.reports, report)
}

type recordingScheduler struct{ ctx context.Context }

func (s *recordingScheduler) Run(
	ctx context.Context,
	_ func(context.Context) error,
	_ func(context.Context) error,
) error {
	s.ctx = ctx
	return ctx.Err()
}

func (r *recordingRunner) Recover(context.Context) error {
	r.calls = append(r.calls, "recover")
	return nil
}

func (r *recordingRunner) Run(_ context.Context, trigger funding.Trigger) error {
	r.calls = append(r.calls, "run:"+string(trigger))
	return nil
}
