package funding

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"hyperliquid-builder-code-bot/internal/hyperliquid/exchange"

	"github.com/shopspring/decimal"
)

func validatePreparedBuilder(state *RunState, builderAddress, expectedKind string, progress *ActionProgress) error {
	if progress.Prepared == nil {
		return fmt.Errorf("persisted builder action request is missing")
	}
	action := *progress.Prepared
	if action.Kind != expectedKind || !strings.EqualFold(action.Signer, builderAddress) {
		return fmt.Errorf("prepared builder action does not match immutable builder")
	}
	if len(action.RequestBody) == 0 {
		return fmt.Errorf("prepared builder request body is empty")
	}
	digest := sha256.Sum256(action.RequestBody)
	if !strings.EqualFold(action.RequestHash, fmt.Sprintf("%x", digest)) {
		return fmt.Errorf("prepared builder request hash mismatch")
	}
	var body struct {
		Action struct {
			Type        string `json:"type"`
			Destination string `json:"destination"`
			Token       string `json:"token"`
			Amount      string `json:"amount"`
			Time        uint64 `json:"time"`
		} `json:"action"`
		Nonce uint64 `json:"nonce"`
	}
	if err := json.Unmarshal(action.RequestBody, &body); err != nil {
		return fmt.Errorf("decode prepared builder request: %w", err)
	}
	if body.Action.Type != expectedKind || body.Nonce != action.Nonce {
		return fmt.Errorf("prepared builder request body does not match persisted action")
	}
	if expectedKind == "claimRewards" {
		if action.Destination != "" || action.Token != "" || action.Amount != "" {
			return fmt.Errorf("prepared claim contains transfer metadata")
		}
		return nil
	}
	if state.Manifest.Token == nil || !strings.EqualFold(action.Destination, state.Manifest.Settlement) || action.Token != state.Manifest.Token.WireToken {
		return fmt.Errorf("prepared builder sweep does not match immutable manifest")
	}
	amount, err := decimal.NewFromString(action.Amount)
	if err != nil || !amount.IsPositive() {
		return fmt.Errorf("prepared builder sweep amount is invalid")
	}
	bodyAmount, err := decimal.NewFromString(body.Action.Amount)
	if err != nil || !bodyAmount.Equal(amount) || !strings.EqualFold(body.Action.Destination, action.Destination) ||
		body.Action.Token != action.Token || body.Action.Time != action.Nonce {
		return fmt.Errorf("prepared builder sweep body does not match persisted action")
	}
	if progress.BalanceBefore != "" {
		before, err := decimal.NewFromString(progress.BalanceBefore)
		if err != nil || !before.Equal(amount) {
			return fmt.Errorf("prepared builder sweep does not transfer the full recorded balance")
		}
	}
	return nil
}

func validatePreparedPayout(state *RunState, action exchange.PreparedAction) error {
	if state.Manifest.Token == nil {
		return fmt.Errorf("payout manifest token is missing")
	}
	if action.Kind != "spotSend" || !strings.EqualFold(action.Signer, state.Manifest.Settlement) ||
		!strings.EqualFold(action.Destination, state.Manifest.Recipient) || action.Token != state.Manifest.Token.WireToken {
		return fmt.Errorf("prepared payout does not match immutable manifest accounts or token")
	}
	preparedAmount, err := decimal.NewFromString(action.Amount)
	if err != nil {
		return fmt.Errorf("parse prepared payout amount: %w", err)
	}
	manifestAmount, err := decimal.NewFromString(state.Manifest.PayoutTotal)
	if err != nil || !preparedAmount.Equal(manifestAmount) {
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
			Type        string `json:"type"`
			Destination string `json:"destination"`
			Token       string `json:"token"`
			Amount      string `json:"amount"`
			Time        uint64 `json:"time"`
		} `json:"action"`
		Nonce uint64 `json:"nonce"`
	}
	if err := json.Unmarshal(action.RequestBody, &body); err != nil {
		return fmt.Errorf("decode prepared payout request: %w", err)
	}
	bodyAmount, err := decimal.NewFromString(body.Action.Amount)
	if err != nil || body.Action.Type != "spotSend" ||
		!strings.EqualFold(body.Action.Destination, action.Destination) || body.Action.Token != action.Token ||
		!bodyAmount.Equal(preparedAmount) || body.Nonce != action.Nonce || body.Action.Time != action.Nonce {
		return fmt.Errorf("prepared payout request body does not match persisted action")
	}
	return nil
}
