package funding

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"hyperliquid-builder-code-bot/internal/hyperliquid/info"

	"github.com/shopspring/decimal"
)

// manifestIdentity is the versioned, immutable input to ManifestHash. Adding,
// removing, renaming, or reordering fields changes the persisted manifest hash
// contract and requires an explicit state compatibility decision.
type manifestIdentity struct {
	Records     []Record    `json:"records"`
	RawTotal    string      `json:"raw_total"`
	PayoutTotal string      `json:"payout_total"`
	Token       *info.Token `json:"token,omitempty"`
	Builders    []string    `json:"builders"`
	Settlement  string      `json:"settlement"`
	Recipient   string      `json:"recipient"`
}

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
		Builders:    append([]string(nil), input.Builders...),
		Settlement:  input.Settlement,
		Recipient:   input.Recipient,
	}
	manifest.ManifestHash, err = HashManifest(manifest)
	if err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func HashManifest(manifest Manifest) (string, error) {
	if !sort.SliceIsSorted(manifest.Records, func(i, j int) bool {
		if manifest.Records[i].PeriodStartAt != manifest.Records[j].PeriodStartAt {
			return manifest.Records[i].PeriodStartAt < manifest.Records[j].PeriodStartAt
		}
		return manifest.Records[i].ID < manifest.Records[j].ID
	}) {
		return "", fmt.Errorf("manifest records are not ordered by period_start_at and id")
	}

	payload := manifestIdentity{
		Records: manifest.Records, RawTotal: manifest.RawTotal,
		PayoutTotal: manifest.PayoutTotal, Token: manifest.Token,
		Builders: manifest.Builders, Settlement: manifest.Settlement,
		Recipient: manifest.Recipient,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode manifest: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
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
