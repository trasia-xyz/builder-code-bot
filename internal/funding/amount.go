package funding

import (
	"errors"
	"fmt"

	"github.com/shopspring/decimal"
)

var ErrNegativeAmount = errors.New("negative funding amount")

func CalculateTotals(records []Record) (rawTotal string, payoutTotal string, err error) {
	total := decimal.Zero
	for _, record := range records {
		amount, parseErr := decimal.NewFromString(record.Amount)
		if parseErr != nil {
			return "", "", fmt.Errorf("parse funding record %d amount: %w", record.ID, parseErr)
		}
		if amount.IsNegative() {
			return "", "", fmt.Errorf("funding record %d: %w", record.ID, ErrNegativeAmount)
		}
		total = total.Add(amount)
	}

	payout := total.Shift(6).Ceil().Shift(-6)
	return total.StringFixed(18), payout.String(), nil
}
