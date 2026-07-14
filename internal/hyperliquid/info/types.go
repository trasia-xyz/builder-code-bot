package info

import "github.com/shopspring/decimal"

type Token struct {
	Name        string `json:"name"`
	TokenID     string `json:"token_id"`
	Index       int    `json:"index"`
	WeiDecimals int    `json:"wei_decimals"`
	WireToken   string `json:"wire_token"`
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

type SpotBalanceAmounts struct {
	Total     decimal.Decimal
	Available decimal.Decimal
}
