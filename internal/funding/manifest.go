package funding

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"

	"hyperliquid-builder-code-bot/internal/hyperliquid/info"

	"github.com/shopspring/decimal"
)

func BuildManifest(input ManifestInput) (Manifest, error) {
	records := append([]Record(nil), input.Records...)
	sort.Slice(records, func(i, j int) bool {
		if records[i].PeriodStartAt != records[j].PeriodStartAt {
			return records[i].PeriodStartAt < records[j].PeriodStartAt
		}
		return records[i].ID < records[j].ID
	})

	rawTotal, payoutTotal, err := CalculateTotals(records)
	if err != nil {
		return Manifest{}, err
	}
	payout, err := decimal.NewFromString(payoutTotal)
	if err != nil {
		return Manifest{}, fmt.Errorf("parse payout total: %w", err)
	}
	if payout.IsPositive() && input.Token == nil {
		return Manifest{}, fmt.Errorf("canonical token is required for a positive payout")
	}

	manifest := Manifest{
		Records:     records,
		RawTotal:    rawTotal,
		PayoutTotal: payoutTotal,
		Token:       cloneToken(input.Token),
		Settlement:  input.Settlement,
		Recipient:   input.Recipient,
	}
	return manifest, nil
}

func NewRunID() (string, error) {
	var value [16]byte
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
