package exchange

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"builder-code-bot/internal/hyperliquid"
	httpclient "builder-code-bot/internal/hyperliquid/client"
	"builder-code-bot/internal/hyperliquid/info"
	"builder-code-bot/internal/hyperliquid/signing"
	"builder-code-bot/internal/secret"

	"github.com/shopspring/decimal"
)

const testPrivateKey = "0x0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestPrepareClaimUsesL1SignatureAndHashesExactBody(t *testing.T) {
	c, address := newTestClient(t, hyperliquid.NetworkTestnet, &recordingTransport{})
	prepared, err := c.PrepareClaim(address, 1720000000123)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Action    json.RawMessage   `json:"action"`
		Nonce     uint64            `json:"nonce"`
		Signature signing.Signature `json:"signature"`
	}
	if err := json.Unmarshal(prepared.RequestBody, &body); err != nil {
		t.Fatal(err)
	}
	if body.Nonce != prepared.Nonce || prepared.Kind != "claimRewards" {
		t.Fatalf("prepared = %#v", prepared)
	}
	if string(body.Action) != `{"type":"claimRewards"}` {
		t.Fatalf("action = %s", body.Action)
	}
	action := signing.Object{signing.F("type", "claimRewards")}
	recovered, err := signing.RecoverL1ActionSigner(signing.L1ActionRecoverInput{Action: action, Nonce: body.Nonce, Network: hyperliquid.NetworkTestnet, Signature: body.Signature})
	if err != nil || !strings.EqualFold(recovered, address) {
		t.Fatalf("recovered = %q, %v", recovered, err)
	}
	digest := sha256.Sum256(prepared.RequestBody)
	if prepared.RequestHash != hex.EncodeToString(digest[:]) {
		t.Fatalf("hash = %q", prepared.RequestHash)
	}
}

func TestPrepareSpotSendUsesTypedSignatureAndActionTimeMatchesNonce(t *testing.T) {
	c, address := newTestClient(t, hyperliquid.NetworkMainnet, &recordingTransport{})
	token := info.Token{Name: "USDC", TokenID: "0xabc", WireToken: "USDC:0xabc", WeiDecimals: 8}
	prepared, err := c.PrepareSpotSend(address, "0x1111111111111111111111111111111111111111", token, decimal.RequireFromString("1.230000000"), 1720000000456)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Action    signing.SpotSendAction `json:"action"`
		Nonce     uint64                 `json:"nonce"`
		Signature signing.Signature      `json:"signature"`
	}
	if err := json.Unmarshal(prepared.RequestBody, &body); err != nil {
		t.Fatal(err)
	}
	if body.Action.Time != body.Nonce || body.Action.Amount != "1.23" || body.Action.Token != token.WireToken {
		t.Fatalf("body = %#v", body)
	}
	recovered, err := signing.RecoverSpotSendSigner(body.Action, body.Signature)
	if err != nil || !strings.EqualFold(recovered, address) {
		t.Fatalf("recovered = %q, %v", recovered, err)
	}
}

func TestSubmitSendsPersistedRequestBodyWithoutRemarshal(t *testing.T) {
	transport := &recordingTransport{response: json.RawMessage(`{"status":"ok","response":{"type":"default"}}`)}
	c, _ := newTestClient(t, hyperliquid.NetworkTestnet, transport)
	body := json.RawMessage("{ \n \"signature\": {\"v\":27}, \"nonce\": 7\n}")
	result, err := c.Submit(context.Background(), PreparedAction{Kind: "claimRewards", RequestBody: body})
	if err != nil {
		t.Fatal(err)
	}
	if string(transport.got) != string(body) {
		t.Fatalf("submitted body = %q, want %q", transport.got, body)
	}
	if !result.Accepted || result.Rejected || string(result.Response) != string(transport.response) {
		t.Fatalf("result = %#v", result)
	}
}

func TestSubmitClassifiesExplicitExchangeRejection(t *testing.T) {
	transport := &recordingTransport{response: json.RawMessage(`{"status":"err","response":"bad nonce"}`)}
	c, _ := newTestClient(t, hyperliquid.NetworkTestnet, transport)
	result, err := c.Submit(context.Background(), PreparedAction{RequestBody: json.RawMessage(`{"nonce":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || !result.Rejected {
		t.Fatalf("result = %#v", result)
	}
}

func TestSubmitTreatsBodyReturnedWithTransportErrorAsUncertain(t *testing.T) {
	for _, status := range []string{"ok", "err"} {
		t.Run(status, func(t *testing.T) {
			body := json.RawMessage(`{"status":"` + status + `","response":"gateway body"}`)
			transport := &recordingTransport{
				response: body,
				err:      errors.New("exchange returned HTTP 503"),
			}
			c, _ := newTestClient(t, hyperliquid.NetworkTestnet, transport)
			result, err := c.Submit(context.Background(), PreparedAction{RequestBody: json.RawMessage(`{"nonce":1}`)})
			if err == nil || result.Accepted || result.Rejected || string(result.Response) != string(body) {
				t.Fatalf("result = %#v, error = %v", result, err)
			}
		})
	}
}

type recordingTransport struct {
	got      json.RawMessage
	response json.RawMessage
	err      error
}

func (t *recordingTransport) ExchangeRaw(_ context.Context, request json.RawMessage, out any) (httpclient.Response, error) {
	t.got = append(json.RawMessage(nil), request...)
	if t.response == nil {
		t.response = json.RawMessage(`{"status":"ok"}`)
	}
	if out != nil {
		_ = json.Unmarshal(t.response, out)
	}
	return httpclient.Response{Body: append([]byte(nil), t.response...)}, t.err
}

func newTestClient(t *testing.T, network hyperliquid.Network, transport Transport) (*Client, string) {
	t.Helper()
	key, err := signing.ParsePrivateKey(secret.NewString(testPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	address, err := key.Address()
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(transport, network, map[string]signing.PrivateKey{address: key})
	if err != nil {
		t.Fatal(err)
	}
	return c, address
}
