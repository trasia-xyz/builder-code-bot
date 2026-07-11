package info

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
	httpclient "hyperliquid-builder-code-bot/internal/hyperliquid/client"
)

type Transport interface {
	Info(ctx context.Context, request any, out any) (httpclient.Response, error)
}

type Client struct{ transport Transport }

func New(transport Transport) *Client { return &Client{transport: transport} }

func (c *Client) SpotMeta(ctx context.Context) (SpotMeta, error) {
	var meta SpotMeta
	if c == nil || c.transport == nil {
		return meta, fmt.Errorf("hyperliquid info transport is nil")
	}
	_, err := c.transport.Info(ctx, map[string]any{"type": "spotMeta"}, &meta)
	if err != nil {
		return meta, fmt.Errorf("query spot metadata: %w", err)
	}
	return meta, nil
}

func (c *Client) CanonicalUSDC(ctx context.Context) (Token, error) {
	meta, err := c.SpotMeta(ctx)
	if err != nil {
		return Token{}, err
	}
	return ResolveCanonicalUSDC(meta)
}

func ResolveCanonicalUSDC(meta SpotMeta) (Token, error) {
	var matches []SpotToken
	for _, token := range meta.Tokens {
		if token.Name == "USDC" && token.IsCanonical {
			matches = append(matches, token)
		}
	}
	if len(matches) != 1 {
		return Token{}, fmt.Errorf("expected exactly one canonical USDC token, found %d", len(matches))
	}
	selected := matches[0]
	if strings.TrimSpace(selected.TokenID) == "" || selected.WeiDecimals < 0 {
		return Token{}, fmt.Errorf("canonical USDC metadata is invalid")
	}
	encoded, err := json.Marshal(selected)
	if err != nil {
		return Token{}, fmt.Errorf("encode canonical USDC metadata: %w", err)
	}
	fingerprint := sha256.Sum256(encoded)
	return Token{
		Name: selected.Name, TokenID: selected.TokenID, Index: selected.Index,
		WeiDecimals: selected.WeiDecimals, WireToken: selected.Name + ":" + selected.TokenID,
		MetaHash: hex.EncodeToString(fingerprint[:]),
	}, nil
}

func AvailableBalance(balance SpotBalance) (decimal.Decimal, error) {
	total, err := decimal.NewFromString(balance.Total)
	if err != nil {
		return decimal.Zero, fmt.Errorf("parse total spot balance: %w", err)
	}
	hold, err := decimal.NewFromString(balance.Hold)
	if err != nil {
		return decimal.Zero, fmt.Errorf("parse held spot balance: %w", err)
	}
	available := total.Sub(hold)
	if available.IsNegative() {
		return decimal.Zero, fmt.Errorf("available spot balance is negative")
	}
	return available, nil
}

func (c *Client) AvailableSpotBalance(ctx context.Context, address string, token Token) (decimal.Decimal, error) {
	if c == nil || c.transport == nil {
		return decimal.Zero, fmt.Errorf("hyperliquid info transport is nil")
	}
	var state struct {
		Balances []SpotBalance `json:"balances"`
	}
	_, err := c.transport.Info(ctx, map[string]any{"type": "spotClearinghouseState", "user": address}, &state)
	if err != nil {
		return decimal.Zero, fmt.Errorf("query spot balance: %w", err)
	}
	for _, balance := range state.Balances {
		if balance.Token == token.Index && balance.Coin == token.Name {
			return AvailableBalance(balance)
		}
	}
	return decimal.Zero, nil
}

func (c *Client) NonFundingLedger(ctx context.Context, address string, start, end uint64) ([]LedgerUpdate, error) {
	if c == nil || c.transport == nil {
		return nil, fmt.Errorf("hyperliquid info transport is nil")
	}
	var updates []LedgerUpdate
	request := map[string]any{"type": "userNonFundingLedgerUpdates", "user": address, "startTime": start, "endTime": end}
	_, err := c.transport.Info(ctx, request, &updates)
	if err != nil {
		return nil, fmt.Errorf("query non-funding ledger: %w", err)
	}
	return updates, nil
}

func MatchSpotTransfer(updates []LedgerUpdate, query TransferQuery) (*LedgerUpdate, bool) {
	if query.StartTime > query.EndTime {
		return nil, false
	}
	var match *LedgerUpdate
	for i := range updates {
		delta := updates[i].Delta
		if updates[i].Time < query.StartTime || updates[i].Time > query.EndTime || delta.Type != "spotTransfer" {
			continue
		}
		if !strings.EqualFold(delta.User, query.Sender) || !strings.EqualFold(delta.Destination, query.Destination) || delta.Token != query.TokenName {
			continue
		}
		amount, err := decimal.NewFromString(string(delta.Amount))
		if err == nil && amount.Equal(query.Amount) {
			if match != nil {
				return nil, false
			}
			match = &updates[i]
		}
	}
	return match, match != nil
}
