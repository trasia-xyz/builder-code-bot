package info

import (
	"context"
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
	return Token{
		Name: selected.Name, TokenID: selected.TokenID, Index: selected.Index,
		WeiDecimals: selected.WeiDecimals, WireToken: selected.Name + ":" + selected.TokenID,
	}, nil
}

func ParseSpotBalance(balance SpotBalance) (SpotBalanceAmounts, error) {
	total, err := decimal.NewFromString(balance.Total)
	if err != nil {
		return SpotBalanceAmounts{}, fmt.Errorf("parse total spot balance: %w", err)
	}
	hold, err := decimal.NewFromString(balance.Hold)
	if err != nil {
		return SpotBalanceAmounts{}, fmt.Errorf("parse held spot balance: %w", err)
	}
	available := total.Sub(hold)
	if available.IsNegative() {
		return SpotBalanceAmounts{}, fmt.Errorf("available spot balance is negative: total %s, hold %s", total, hold)
	}
	return SpotBalanceAmounts{Total: total, Available: available}, nil
}

func (c *Client) SpotBalance(ctx context.Context, address string, token Token) (SpotBalanceAmounts, error) {
	if c == nil || c.transport == nil {
		return SpotBalanceAmounts{}, fmt.Errorf("hyperliquid info transport is nil")
	}
	var state struct {
		Balances []SpotBalance `json:"balances"`
	}
	_, err := c.transport.Info(ctx, map[string]any{"type": "spotClearinghouseState", "user": address}, &state)
	if err != nil {
		return SpotBalanceAmounts{}, fmt.Errorf("query spot balance: %w", err)
	}
	for _, balance := range state.Balances {
		if balance.Token == token.Index && balance.Coin == token.Name {
			return ParseSpotBalance(balance)
		}
	}
	return SpotBalanceAmounts{Total: decimal.Zero, Available: decimal.Zero}, nil
}
