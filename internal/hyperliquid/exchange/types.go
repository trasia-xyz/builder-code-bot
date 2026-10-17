package exchange

import "encoding/json"

type PreparedAction struct {
	Kind        string          `json:"kind"`
	Signer      string          `json:"signer"`
	Destination string          `json:"destination,omitempty"`
	Token       string          `json:"token,omitempty"`
	Amount      string          `json:"amount,omitempty"`
	Nonce       uint64          `json:"nonce"`
	RequestBody json.RawMessage `json:"request_body"`
}

type SubmitResult struct {
	Accepted bool            `json:"accepted"`
	Rejected bool            `json:"rejected"`
	Response json.RawMessage `json:"response"`
}

// RejectionMessage returns the exchange's top-level string rejection. It only
// accepts the documented {"status":"err","response":"..."} envelope so
// callers never classify a rejection from a raw substring match.
func (r SubmitResult) RejectionMessage() (string, bool) {
	if !r.Rejected || len(r.Response) == 0 {
		return "", false
	}
	var envelope struct {
		Status   string          `json:"status"`
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(r.Response, &envelope); err != nil || envelope.Status != "err" {
		return "", false
	}
	var message string
	if err := json.Unmarshal(envelope.Response, &message); err != nil {
		return "", false
	}
	return message, true
}
