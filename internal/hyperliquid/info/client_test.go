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
	if got.WireToken != "USDC:0xabc" || got.MetaHash == "" {
		t.Fatalf("token = %#v", got)
	}
	meta.Tokens = append(meta.Tokens, SpotToken{Name: "USDC", TokenID: "0xdef", IsCanonical: true})
	if _, err := ResolveCanonicalUSDC(meta); err == nil {
		t.Fatal("expected ambiguous token error")
	}
}

func TestAvailableBalanceSubtractsHold(t *testing.T) {
	got, err := AvailableBalance(SpotBalance{Total: "12.5", Hold: "2.25"})
	if err != nil || !got.Equal(decimal.RequireFromString("10.25")) {
		t.Fatalf("AvailableBalance() = %s, %v", got, err)
	}
	if _, err := AvailableBalance(SpotBalance{Total: "1", Hold: "2"}); err == nil {
		t.Fatal("expected negative available balance error")
	}
}

func TestInfoClientQueriesMetadataBalancesAndLedger(t *testing.T) {
	transport := &fakeTransport{responses: []json.RawMessage{
		json.RawMessage(`{"tokens":[{"name":"USDC","tokenId":"0xabc","index":0,"weiDecimals":8,"isCanonical":true}]}`),
		json.RawMessage(`{"balances":[{"coin":"USDC","token":0,"total":"4.5","hold":"0.5"}]}`),
		json.RawMessage(`[{"time":10,"hash":"0x1","delta":{"type":"spotTransfer","token":"USDC","amount":4,"user":"0xsender","destination":"0xdest","fee":"0"}}]`),
	}}
	c := New(transport)
	token, err := c.CanonicalUSDC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	balance, err := c.AvailableSpotBalance(context.Background(), "0xsender", token)
	if err != nil || !balance.Equal(decimal.NewFromInt(4)) {
		t.Fatalf("balance = %s, %v", balance, err)
	}
	updates, err := c.NonFundingLedger(context.Background(), "0xsender", 1, 20)
	if err != nil || len(updates) != 1 {
		t.Fatalf("updates = %#v, %v", updates, err)
	}
	if updates[0].Delta.Amount != "4" {
		t.Fatalf("numeric amount = %q, want 4", updates[0].Delta.Amount)
	}
	if got := transport.requests[1]["type"]; got != "spotClearinghouseState" {
		t.Fatalf("balance request type = %v", got)
	}
}

func TestMatchSpotTransferRequiresUniqueExactRecordInsideTimeWindow(t *testing.T) {
	query := TransferQuery{
		Sender: "0xAbC", Destination: "0xDeF", TokenName: "USDC",
		Amount: decimal.RequireFromString("1.2300"), ActionTime: 10_000,
		StartTime: 9_000, EndTime: 12_000,
	}
	match := LedgerUpdate{Time: 10_500, Delta: SpotTransferDelta{Type: "spotTransfer", Token: "USDC", Amount: "1.23", User: "0xabc", Destination: "0xdef"}}
	if _, ok := MatchSpotTransfer([]LedgerUpdate{match}, query); !ok {
		t.Fatal("expected exact decimal transfer match")
	}
	match.Time = query.StartTime - 1
	if _, ok := MatchSpotTransfer([]LedgerUpdate{match}, query); ok {
		t.Fatal("record before reconciliation window must not match")
	}
	match.Time = query.EndTime + 1
	if _, ok := MatchSpotTransfer([]LedgerUpdate{match}, query); ok {
		t.Fatal("record after reconciliation window must not match")
	}
	match.Time = 10_500
	duplicate := match
	duplicate.Time++
	if _, ok := MatchSpotTransfer([]LedgerUpdate{match, duplicate}, query); ok {
		t.Fatal("multiple indistinguishable transfers must not produce exact evidence")
	}

	tests := []struct {
		name   string
		mutate func(*LedgerUpdate)
	}{
		{name: "type", mutate: func(update *LedgerUpdate) { update.Delta.Type = "withdraw" }},
		{name: "sender", mutate: func(update *LedgerUpdate) { update.Delta.User = "0xother" }},
		{name: "destination", mutate: func(update *LedgerUpdate) { update.Delta.Destination = "0xother" }},
		{name: "token", mutate: func(update *LedgerUpdate) { update.Delta.Token = "USDT" }},
		{name: "amount", mutate: func(update *LedgerUpdate) { update.Delta.Amount = "1.2301" }},
		{name: "invalid amount", mutate: func(update *LedgerUpdate) { update.Delta.Amount = "secret-invalid" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := LedgerUpdate{Time: 10_500, Delta: SpotTransferDelta{Type: "spotTransfer", Token: "USDC", Amount: "1.23", User: "0xabc", Destination: "0xdef"}}
			tt.mutate(&candidate)
			if _, ok := MatchSpotTransfer([]LedgerUpdate{candidate}, query); ok {
				t.Fatalf("mismatched update = %#v", candidate)
			}
		})
	}
}

func TestInfoMethodsRejectNilTransport(t *testing.T) {
	c := New(nil)
	if _, err := c.AvailableSpotBalance(context.Background(), "0xsender", Token{}); err == nil {
		t.Fatal("expected nil transport error")
	}
	if _, err := c.NonFundingLedger(context.Background(), "0xsender", 1, 2); err == nil {
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
