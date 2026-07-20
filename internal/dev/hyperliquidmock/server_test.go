package hyperliquidmock

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"builder-code-bot/internal/hyperliquid"
	httpclient "builder-code-bot/internal/hyperliquid/client"
	"builder-code-bot/internal/hyperliquid/exchange"
	"builder-code-bot/internal/hyperliquid/info"
	"builder-code-bot/internal/hyperliquid/signing"
	"builder-code-bot/internal/secret"
)

const mockPrivateKey = "0x0000000000000000000000000000000000000000000000000000000000000001"

func TestServerProvidesSpotMetadataAndBalances(t *testing.T) {
	server := New(t)
	server.SetSpotBalance("0xabc", "USDC:0", "12.5")

	transport, err := httpclient.New(httpclient.Config{Network: hyperliquid.NetworkTestnet, BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	client := info.New(transport)
	token, err := client.CanonicalUSDC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token.WireToken != "USDC:0" || token.WeiDecimals != 6 {
		t.Fatalf("token = %#v", token)
	}
	balance, err := client.SpotBalance(context.Background(), "0xAbC", token)
	if err != nil {
		t.Fatal(err)
	}
	if !balance.Total.Equal(decimal.RequireFromString("12.5")) || !balance.Available.Equal(decimal.RequireFromString("12.5")) {
		t.Fatalf("balance = %+v, want total and available 12.5", balance)
	}
	claimable, err := client.ClaimableUSDC(context.Background(), "0x000000000000000000000000000000000000dEaD", token)
	if err != nil || !claimable.IsZero() {
		t.Fatalf("claimable USDC without reward = %s, %v, want 0", claimable, err)
	}
	server.SetClaimReward("0xabc", "USDC:0", "0.75")
	claimable, err = client.ClaimableUSDC(context.Background(), "0xAbC", token)
	if err != nil {
		t.Fatal(err)
	}
	if !claimable.Equal(decimal.RequireFromString("0.75")) {
		t.Fatalf("claimable USDC = %s, want 0.75", claimable)
	}
	server.SetUserRateLimit("0xabc", 9801, 10000)
	limit, err := client.UserRateLimit(context.Background(), "0xAbC")
	if err != nil {
		t.Fatal(err)
	}
	if limit.RemainingRequests() != 199 {
		t.Fatalf("remaining requests = %d, want 199", limit.RemainingRequests())
	}
}

func TestServerAppliesSpotSendOnceForSameSignedRequest(t *testing.T) {
	server := New(t)
	client, signer := newExchangeClient(t, server.URL)
	server.SetSpotBalance(signer, "USDC:0", "5")
	action, err := client.PrepareSpotSend(signer, "0x00000000000000000000000000000000000000aa", testToken(), decimal.RequireFromString("2.25"), 1001)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		result, err := client.Submit(context.Background(), action)
		if err != nil || !result.Accepted {
			t.Fatalf("Submit() = %#v, %v", result, err)
		}
	}

	assertBalance(t, server, signer, "2.75")
	assertBalance(t, server, "0x00000000000000000000000000000000000000aa", "2.25")
	requests := server.Requests()
	exchangeRequests := 0
	for _, request := range requests {
		if request.ActionType == "spotSend" {
			exchangeRequests++
		}
	}
	if exchangeRequests != 2 {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestServerRejectsConflictingRequestWithSameSignerAndNonce(t *testing.T) {
	server := New(t)
	client, signer := newExchangeClient(t, server.URL)
	server.SetSpotBalance(signer, "USDC:0", "5")
	first, err := client.PrepareSpotSend(signer, "0x00000000000000000000000000000000000000aa", testToken(), decimal.NewFromInt(1), 1002)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.PrepareSpotSend(signer, "0x00000000000000000000000000000000000000bb", testToken(), decimal.NewFromInt(1), 1002)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := client.Submit(context.Background(), first); err != nil || !result.Accepted {
		t.Fatalf("first Submit() = %#v, %v", result, err)
	}
	if result, err := client.Submit(context.Background(), second); err != nil || !result.Rejected {
		t.Fatalf("second Submit() = %#v, %v", result, err)
	}
	assertBalance(t, server, signer, "4")
}

func TestServerFailureModesDistinguishRejectedAndAmbiguousApplied(t *testing.T) {
	server := New(t)
	client, signer := newExchangeClient(t, server.URL)
	server.SetSpotBalance(signer, "USDC:0", "5")

	server.FailNextExchange(FailureRejected)
	rejected, err := client.PrepareSpotSend(signer, "0x00000000000000000000000000000000000000aa", testToken(), decimal.NewFromInt(1), 1003)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := client.Submit(context.Background(), rejected); err != nil || !result.Rejected {
		t.Fatalf("rejected Submit() = %#v, %v", result, err)
	}
	assertBalance(t, server, signer, "5")

	server.FailNextExchange(FailureAmbiguousApplied)
	ambiguous, err := client.PrepareSpotSend(signer, "0x00000000000000000000000000000000000000aa", testToken(), decimal.NewFromInt(2), 1004)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := client.Submit(context.Background(), ambiguous); err == nil || result.Accepted || result.Rejected {
		t.Fatalf("ambiguous Submit() = %#v, %v", result, err)
	}
	assertBalance(t, server, signer, "3")
}

func TestServerCreditsConfiguredClaimReward(t *testing.T) {
	server := New(t)
	client, signer := newExchangeClient(t, server.URL)
	server.SetSpotBalance(signer, "USDC:0", "0.5")
	server.SetClaimReward(signer, "USDC:0", "1.25")
	action, err := client.PrepareClaim(signer, 1005)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := client.Submit(context.Background(), action); err != nil || !result.Accepted {
		t.Fatalf("Submit() = %#v, %v", result, err)
	}
	assertBalance(t, server, signer, "1.75")
}

func TestServerRejectsClaimRewardAtThreshold(t *testing.T) {
	server := New(t)
	client, signer := newExchangeClient(t, server.URL)
	server.SetClaimReward(signer, "USDC:0", "1")
	action, err := client.PrepareClaim(signer, 1006)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := client.Submit(context.Background(), action); err != nil || !result.Rejected {
		t.Fatalf("Submit() = %#v, %v, want explicit rejection", result, err)
	}
}

func TestServerFailureModesControlWhetherMutationApplies(t *testing.T) {
	tests := []struct {
		name string
		mode FailureMode
		want string
	}{
		{name: "ambiguous unapplied", mode: FailureAmbiguous, want: "5"},
		{name: "HTTP error unapplied", mode: FailureHTTPError, want: "5"},
		{name: "HTTP error applied", mode: FailureHTTPErrorApplied, want: "4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := New(t)
			client, signer := newExchangeClient(t, server.URL)
			server.SetSpotBalance(signer, "USDC:0", "5")
			server.FailNextExchange(tt.mode)
			action, err := client.PrepareSpotSend(signer, "0x00000000000000000000000000000000000000aa", testToken(), decimal.NewFromInt(1), 1006)
			if err != nil {
				t.Fatal(err)
			}
			if result, err := client.Submit(context.Background(), action); err == nil || result.Accepted || result.Rejected {
				t.Fatalf("Submit() = %#v, %v", result, err)
			}
			assertBalance(t, server, signer, tt.want)
		})
	}
}

func TestServerRejectsSpotSendForDifferentConfiguredNetwork(t *testing.T) {
	server := New(t)
	client, signer := newExchangeClientForNetwork(t, server.URL, hyperliquid.NetworkMainnet)
	server.SetSpotBalance(signer, "USDC:0", "5")
	action, err := client.PrepareSpotSend(signer, "0x00000000000000000000000000000000000000aa", testToken(), decimal.NewFromInt(1), 1007)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := client.Submit(context.Background(), action); err != nil || !result.Rejected {
		t.Fatalf("Submit() = %#v, %v", result, err)
	}
	assertBalance(t, server, signer, "5")
}

func newExchangeClient(t *testing.T, baseURL string) (*exchange.Client, string) {
	return newExchangeClientForNetwork(t, baseURL, hyperliquid.NetworkTestnet)
}

func newExchangeClientForNetwork(t *testing.T, baseURL string, network hyperliquid.Network) (*exchange.Client, string) {
	t.Helper()
	key, err := signing.ParsePrivateKey(secret.NewString(mockPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	address, err := key.Address()
	if err != nil {
		t.Fatal(err)
	}
	transport, err := httpclient.New(httpclient.Config{Network: network, BaseURL: baseURL})
	if err != nil {
		t.Fatal(err)
	}
	client, err := exchange.New(transport, network, map[string]signing.PrivateKey{address: key})
	if err != nil {
		t.Fatal(err)
	}
	return client, address
}

func testToken() info.Token {
	return info.Token{Name: "USDC", TokenID: "0", Index: 0, WeiDecimals: 6, WireToken: "USDC:0"}
}

func assertBalance(t *testing.T, server *Server, address, want string) {
	t.Helper()
	transport, err := httpclient.New(httpclient.Config{Network: hyperliquid.NetworkTestnet, BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	got, err := info.New(transport).SpotBalance(context.Background(), address, testToken())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Available.Equal(decimal.RequireFromString(want)) {
		t.Fatalf("balance for %s = %s, want %s", address, got.Available, want)
	}
}
