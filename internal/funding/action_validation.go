package funding

import (
	"encoding/json"
	"fmt"
	"strings"

	"builder-code-bot/internal/hyperliquid/exchange"

	"github.com/shopspring/decimal"
)

func validatePreparedPayout(target *PayoutTargetState, action exchange.PreparedAction) error {
	if target == nil {
		return fmt.Errorf("payout target is missing")
	}
	if target.Manifest.Token == nil {
		return fmt.Errorf("payout manifest token is missing")
	}
	if action.Kind != "spotSend" || !strings.EqualFold(action.Signer, target.Manifest.Settlement) ||
		!strings.EqualFold(action.Destination, target.Manifest.Recipient) || action.Token != target.Manifest.Token.WireToken {
		return fmt.Errorf("prepared payout does not match immutable manifest")
	}
	amount, err := decimal.NewFromString(action.Amount)
	want, wantErr := decimal.NewFromString(target.Manifest.PayoutTotal)
	if err != nil || wantErr != nil || !amount.Equal(want) {
		return fmt.Errorf("prepared payout amount does not match immutable manifest")
	}
	if len(action.RequestBody) == 0 {
		return fmt.Errorf("prepared payout request body is empty")
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

func validatePayoutJournal(target *PayoutTargetState) error {
	if target == nil || target.Payout == nil {
		return fmt.Errorf("payout journal is missing")
	}
	if err := validatePreparedPayout(target, target.Payout.Prepared); err != nil {
		return err
	}
	before, err := decimal.NewFromString(target.Payout.TotalBefore)
	if err != nil || before.IsNegative() {
		return fmt.Errorf("payout settlement total-before boundary is invalid")
	}
	amount, err := decimal.NewFromString(target.Manifest.PayoutTotal)
	if err != nil || !amount.IsPositive() {
		return fmt.Errorf("payout amount is invalid")
	}
	if before.LessThan(amount) {
		return fmt.Errorf("payout settlement total-before boundary is below payout amount")
	}
	if target.Payout.SubmitAttempts < 1 || target.Payout.SubmitAttempts > 2 {
		return fmt.Errorf("payout submit attempts must be one or two")
	}
	return nil
}

func validateConfirmedPayoutState(target *PayoutTargetState) error {
	return validatePayoutJournal(target)
}

func validateTargetProgress(state *RunState, active *PayoutTargetState) error {
	resolved, _, err := state.activePayoutTarget()
	if err != nil {
		return err
	}
	if resolved == nil || resolved != active {
		return fmt.Errorf("active payout target does not match persisted progress")
	}
	if active.Payout != nil {
		if err := validatePayoutJournal(active); err != nil {
			return err
		}
		switch active.Phase {
		case PhasePayoutPrepared, PhasePayoutSubmitting:
			if active.Payout.SubmitAttempts != 1 {
				return fmt.Errorf("%s requires exactly one reserved submit attempt", active.Phase)
			}
		case PhasePayoutReplaying:
			if active.Payout.SubmitAttempts != 2 {
				return fmt.Errorf("payout replaying requires exactly two reserved submit attempts")
			}
		}
	}
	return nil
}
