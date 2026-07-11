package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"hyperliquid-builder-code-bot/internal/config"
	"hyperliquid-builder-code-bot/internal/crypt/keycipher"
	"hyperliquid-builder-code-bot/internal/funding"
	httpclient "hyperliquid-builder-code-bot/internal/hyperliquid/client"
	"hyperliquid-builder-code-bot/internal/hyperliquid/info"
	"hyperliquid-builder-code-bot/internal/hyperliquid/signing"
	"hyperliquid-builder-code-bot/internal/secret"
)

func TestHyperliquidChainQueriesConfiguredLedgerReconciliationWindow(t *testing.T) {
	transport := &ledgerRecordingTransport{response: json.RawMessage(`[{"time":1100,"delta":{"type":"spotTransfer","token":"USDC","amount":"1.25","user":"0xsender","destination":"0xdestination"}}]`)}
	chain := hyperliquidChain{info: info.New(transport)}
	query := info.TransferQuery{
		Sender: "0xsender", Destination: "0xdestination", TokenName: "USDC",
		Amount: decimal.RequireFromString("1.25"), ActionTime: 1000,
		StartTime: 500, EndTime: 5_000,
	}

	update, matched, err := chain.FindSpotTransfer(context.Background(), query)
	if err != nil || !matched || update == nil {
		t.Fatalf("FindSpotTransfer() = %#v, %v, %v", update, matched, err)
	}
	if transport.request["startTime"] != uint64(500) || transport.request["endTime"] != uint64(5_000) {
		t.Fatalf("ledger request = %#v", transport.request)
	}
}

type ledgerRecordingTransport struct {
	request  map[string]any
	response json.RawMessage
}

func (t *ledgerRecordingTransport) Info(_ context.Context, request any, out any) (httpclient.Response, error) {
	t.request, _ = request.(map[string]any)
	return httpclient.Response{Body: t.response}, json.Unmarshal(t.response, out)
}

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

func TestRuntimeRecoversBeforeOptionalRunOnStart(t *testing.T) {
	runner := &recordingRunner{}
	if err := startRuntime(context.Background(), runner, true); err != nil {
		t.Fatalf("startRuntime() error = %v", err)
	}
	want := []string{"recover", "run:run_on_start"}
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

func (r *recordingRunner) Recover(context.Context) error {
	r.calls = append(r.calls, "recover")
	return nil
}

func (r *recordingRunner) RunNew(_ context.Context, trigger funding.Trigger) error {
	r.calls = append(r.calls, "run:"+string(trigger))
	return nil
}
