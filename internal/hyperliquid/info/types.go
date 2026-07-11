package info

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"
)

type Token struct {
	Name        string `json:"name"`
	TokenID     string `json:"token_id"`
	Index       int    `json:"index"`
	WeiDecimals int    `json:"wei_decimals"`
	WireToken   string `json:"wire_token"`
	MetaHash    string `json:"meta_hash"`
}

type SpotMeta struct {
	Tokens []SpotToken `json:"tokens"`
}

type SpotToken struct {
	Name        string `json:"name"`
	TokenID     string `json:"tokenId"`
	Index       int    `json:"index"`
	WeiDecimals int    `json:"weiDecimals"`
	IsCanonical bool   `json:"isCanonical"`
}

type SpotBalance struct {
	Coin  string `json:"coin"`
	Token int    `json:"token"`
	Total string `json:"total"`
	Hold  string `json:"hold"`
}

type LedgerUpdate struct {
	Time  uint64            `json:"time"`
	Hash  string            `json:"hash"`
	Delta SpotTransferDelta `json:"delta"`
}

type SpotTransferDelta struct {
	Type        string      `json:"type"`
	Token       string      `json:"token"`
	Amount      DecimalText `json:"amount"`
	USDCValue   DecimalText `json:"usdcValue,omitempty"`
	User        string      `json:"user"`
	Destination string      `json:"destination"`
	Fee         DecimalText `json:"fee,omitempty"`
}

type TransferQuery struct {
	Sender      string
	Destination string
	TokenName   string
	Amount      decimal.Decimal
	ActionTime  uint64
	StartTime   uint64
	EndTime     uint64
}

// DecimalText accepts the string and JSON-number encodings emitted by
// Hyperliquid while retaining exact decimal text for later comparison.
type DecimalText string

func (d *DecimalText) UnmarshalJSON(data []byte) error {
	if d == nil {
		return fmt.Errorf("decode decimal into nil destination")
	}
	var text string
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
	} else {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var number json.Number
		if err := decoder.Decode(&number); err != nil {
			return fmt.Errorf("decode decimal number: %w", err)
		}
		text = number.String()
	}
	if _, err := decimal.NewFromString(text); err != nil {
		return fmt.Errorf("decode decimal value: %w", err)
	}
	*d = DecimalText(text)
	return nil
}
