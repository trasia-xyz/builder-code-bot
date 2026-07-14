package funding

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"

	"builder-code-bot/internal/hyperliquid/info"

	"github.com/shopspring/decimal"
)

func BuildManifest(input ManifestInput) (Manifest, error) {
	return buildManifest(input, true)
}

func buildManifest(input ManifestInput, requireToken bool) (Manifest, error) {
	records := append([]Record(nil), input.Records...)
	sort.Slice(records, func(i, j int) bool {
		if records[i].PeriodStartAt != records[j].PeriodStartAt {
			return records[i].PeriodStartAt < records[j].PeriodStartAt
		}
		return records[i].ID < records[j].ID
	})

	manifest := Manifest{
		Records: records, Settlement: input.Settlement, Recipient: input.Recipient,
	}
	rawTotal, payoutTotal, err := CalculateTotals(records)
	if err != nil {
		return manifest, err
	}
	payout, err := decimal.NewFromString(payoutTotal)
	if err != nil {
		return manifest, fmt.Errorf("parse payout total: %w", err)
	}
	if requireToken && payout.IsPositive() && input.Token == nil {
		return manifest, fmt.Errorf("canonical token is required for a positive payout")
	}
	manifest.RawTotal = rawTotal
	manifest.PayoutTotal = payoutTotal
	manifest.Token = cloneToken(input.Token)
	return manifest, nil
}

func NewRunID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate run ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func cloneToken(token *info.Token) *info.Token {
	if token == nil {
		return nil
	}
	copy := *token
	return &copy
}
