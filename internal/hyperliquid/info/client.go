package info

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	httpclient "builder-code-bot/internal/hyperliquid/client"

	"github.com/shopspring/decimal"
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

func (c *Client) ClaimableUSDC(ctx context.Context, address string, token Token) (decimal.Decimal, error) {
	if c == nil || c.transport == nil {
		return decimal.Zero, fmt.Errorf("hyperliquid info transport is nil")
	}
	var response struct {
		TokenToState *[][]json.RawMessage `json:"tokenToState"`
	}
	_, err := c.transport.Info(ctx, map[string]any{"type": "referral", "user": address}, &response)
	if err != nil {
		return decimal.Zero, fmt.Errorf("query claimable USDC rewards: %w", err)
	}
	if response.TokenToState == nil {
		return decimal.Zero, fmt.Errorf("claimable reward response is missing tokenToState")
	}
	var (
		claimable decimal.Decimal
		found     bool
	)
	for entryIndex, entry := range *response.TokenToState {
		if len(entry) != 2 {
			return decimal.Zero, fmt.Errorf("claimable reward token entry %d is invalid", entryIndex)
		}
		var tokenIndex int
		if err := json.Unmarshal(entry[0], &tokenIndex); err != nil {
			return decimal.Zero, fmt.Errorf("parse claimable reward token index at entry %d: %w", entryIndex, err)
		}
		if tokenIndex != token.Index {
			continue
		}
		if found {
			return decimal.Zero, fmt.Errorf("claimable reward response contains duplicate USDC token index %d", token.Index)
		}
		var state struct {
			UnclaimedRewards string `json:"unclaimedRewards"`
		}
		if err := json.Unmarshal(entry[1], &state); err != nil {
			return decimal.Zero, fmt.Errorf("parse claimable USDC reward state: %w", err)
		}
		claimable, err = decimal.NewFromString(state.UnclaimedRewards)
		if err != nil {
			return decimal.Zero, fmt.Errorf("parse claimable USDC rewards: %w", err)
		}
		if claimable.IsNegative() {
			return decimal.Zero, fmt.Errorf("claimable USDC rewards are negative: %s", claimable)
		}
		found = true
	}
	if !found {
		return decimal.Zero, nil
	}
	return claimable, nil
}

func (c *Client) UserRateLimit(ctx context.Context, address string) (UserRateLimit, error) {
	if c == nil || c.transport == nil {
		return UserRateLimit{}, fmt.Errorf("hyperliquid info transport is nil")
	}
	var response struct {
		CumVlm           string  `json:"cumVlm"`
		NRequestsUsed    *uint64 `json:"nRequestsUsed"`
		NRequestsCap     *uint64 `json:"nRequestsCap"`
		NRequestsSurplus uint64  `json:"nRequestsSurplus"`
	}
	_, err := c.transport.Info(ctx, map[string]any{"type": "userRateLimit", "user": address}, &response)
	if err != nil {
		return UserRateLimit{}, fmt.Errorf("query user rate limit: %w", err)
	}
	if response.NRequestsUsed == nil || response.NRequestsCap == nil {
		return UserRateLimit{}, fmt.Errorf("query user rate limit: response is missing request counts")
	}
	return UserRateLimit{
		CumVlm:           response.CumVlm,
		NRequestsUsed:    *response.NRequestsUsed,
		NRequestsCap:     *response.NRequestsCap,
		NRequestsSurplus: response.NRequestsSurplus,
	}, nil
}
