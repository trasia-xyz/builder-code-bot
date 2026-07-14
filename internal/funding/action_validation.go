package funding

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"hyperliquid-builder-code-bot/internal/hyperliquid/exchange"

	"github.com/shopspring/decimal"
)

func validatePreparedPayout(state *RunState, action exchange.PreparedAction) error {
	if state.Manifest.Token == nil {
		return fmt.Errorf("payout manifest token is missing")
	}
	if action.Kind != "spotSend" || !strings.EqualFold(action.Signer, state.Manifest.Settlement) ||
		!strings.EqualFold(action.Destination, state.Manifest.Recipient) || action.Token != state.Manifest.Token.WireToken {
		return fmt.Errorf("prepared payout does not match immutable manifest")
	}
	amount, err := decimal.NewFromString(action.Amount)
	want, wantErr := decimal.NewFromString(state.Manifest.PayoutTotal)
	if err != nil || wantErr != nil || !amount.Equal(want) {
		return fmt.Errorf("prepared payout amount does not match immutable manifest")
	}
	if len(action.RequestBody) == 0 {
		return fmt.Errorf("prepared payout request body is empty")
	}
	digest := sha256.Sum256(action.RequestBody)
	if !strings.EqualFold(action.RequestHash, fmt.Sprintf("%x", digest)) {
		return fmt.Errorf("prepared payout request hash mismatch")
	}
	var body struct {
		Action struct {
			Type, Destination, Token, Amount string
			Time                             uint64
		} `json:"action"`
		Nonce uint64 `json:"nonce"`
	}
	if err := json.Unmarshal(action.RequestBody, &body); err != nil {
		return fmt.Errorf("decode prepared payout request: %w", err)
	}
	bodyAmount, bodyErr := decimal.NewFromString(body.Action.Amount)
	if bodyErr != nil || body.Action.Type != "spotSend" ||
		!strings.EqualFold(body.Action.Destination, action.Destination) || body.Action.Token != action.Token ||
		!bodyAmount.Equal(amount) || body.Nonce != action.Nonce || body.Action.Time != action.Nonce {
		return fmt.Errorf("prepared payout request body does not match persisted action")
	}
	return nil
}
