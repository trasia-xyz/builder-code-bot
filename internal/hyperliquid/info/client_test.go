package info

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"
	httpclient "hyperliquid-builder-code-bot/internal/hyperliquid/client"
)

func TestResolveCanonicalUSDCRequiresUniqueCanonicalToken(t *testing.T) {
	meta := SpotMeta{Tokens: []SpotToken{{Name: "USDC", TokenID: "0xabc", Index: 0, WeiDecimals: 8, IsCanonical: true}}}
	got, err := ResolveCanonicalUSDC(meta)
	if err != nil {
		t.Fatal(err)
	}
	if got.WireToken != "USDC:0xabc" {
		t.Fatalf("token = %#v", got)
	}
	meta.Tokens = append(meta.Tokens, SpotToken{Name: "USDC", TokenID: "0xdef", IsCanonical: true})
	if _, err := ResolveCanonicalUSDC(meta); err == nil {
		t.Fatal("expected ambiguous token error")
	}
}

func TestParseSpotBalancePreservesTotalAndSubtractsHold(t *testing.T) {
	got, err := ParseSpotBalance(SpotBalance{Total: "12.5", Hold: "2.25"})
	if err != nil || !got.Total.Equal(decimal.RequireFromString("12.5")) || !got.Available.Equal(decimal.RequireFromString("10.25")) {
		t.Fatalf("ParseSpotBalance() = %+v, %v", got, err)
	}
	if _, err := ParseSpotBalance(SpotBalance{Total: "1", Hold: "2"}); err == nil {
		t.Fatal("expected negative available balance error")
	}
	if _, err := ParseSpotBalance(SpotBalance{Total: "bad", Hold: "0"}); err == nil {
		t.Fatal("expected invalid total error")
	}
	if _, err := ParseSpotBalance(SpotBalance{Total: "1", Hold: "bad"}); err == nil {
		t.Fatal("expected invalid hold error")
	}
}

func TestInfoClientQueriesMetadataAndBalances(t *testing.T) {
	transport := &fakeTransport{responses: []json.RawMessage{
		json.RawMessage(`{"tokens":[{"name":"USDC","tokenId":"0xabc","index":0,"weiDecimals":8,"isCanonical":true}]}`),
		json.RawMessage(`{"balances":[{"coin":"USDC","token":0,"total":"4.5","hold":"0.5"}]}`),
	}}
	c := New(transport)
	token, err := c.CanonicalUSDC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	balance, err := c.SpotBalance(context.Background(), "0xsender", token)
	if err != nil || !balance.Total.Equal(decimal.RequireFromString("4.5")) || !balance.Available.Equal(decimal.NewFromInt(4)) {
		t.Fatalf("balance = %+v, %v", balance, err)
	}
	if got := transport.requests[1]["type"]; got != "spotClearinghouseState" {
		t.Fatalf("balance request type = %v", got)
	}
}

func TestInfoMethodsRejectNilTransport(t *testing.T) {
	c := New(nil)
	if _, err := c.SpotBalance(context.Background(), "0xsender", Token{}); err == nil {
		t.Fatal("expected nil transport error")
	}
}

type fakeTransport struct {
	responses []json.RawMessage
	requests  []map[string]any
}

func (f *fakeTransport) Info(_ context.Context, request any, out any) (httpclient.Response, error) {
	body, _ := json.Marshal(request)
	var decoded map[string]any
	_ = json.Unmarshal(body, &decoded)
	f.requests = append(f.requests, decoded)
	response := f.responses[0]
	f.responses = f.responses[1:]
	if err := json.Unmarshal(response, out); err != nil {
		return httpclient.Response{}, err
	}
	return httpclient.Response{Body: response}, nil
}
