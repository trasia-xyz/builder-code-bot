package exchange

import "encoding/json"

type PreparedAction struct {
	Kind        string          `json:"kind"`
	Signer      string          `json:"signer"`
	Destination string          `json:"destination,omitempty"`
	Token       string          `json:"token,omitempty"`
	Amount      string          `json:"amount,omitempty"`
	Nonce       uint64          `json:"nonce"`
	RequestHash string          `json:"request_hash"`
	RequestBody json.RawMessage `json:"request_body"`
}

type SubmitResult struct {
	Accepted bool            `json:"accepted"`
	Rejected bool            `json:"rejected"`
	Response json.RawMessage `json:"response"`
}
