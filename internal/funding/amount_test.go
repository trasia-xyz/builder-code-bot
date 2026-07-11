package funding

import (
	"errors"
	"testing"
)

func TestCalculateTotalsCeilsAggregateOnce(t *testing.T) {
	records := []Record{
		{ID: 1, Amount: "1.000000000000000001"},
		{ID: 2, Amount: "0.123456000000000000"},
	}

	raw, payout, err := CalculateTotals(records)
	if err != nil {
		t.Fatal(err)
	}
	if raw != "1.123456000000000001" || payout != "1.123457" {
		t.Fatalf("CalculateTotals() = %s, %s", raw, payout)
	}
}

func TestCalculateTotalsRejectsAnyNegative(t *testing.T) {
	_, _, err := CalculateTotals([]Record{
		{ID: 1, Amount: "2.000000000000000000"},
		{ID: 2, Amount: "-1.000000000000000000"},
	})
	if !errors.Is(err, ErrNegativeAmount) {
		t.Fatalf("CalculateTotals() error = %v", err)
	}
}

func TestCalculateTotalsFormatsEmptyTotal(t *testing.T) {
	raw, payout, err := CalculateTotals(nil)
	if err != nil {
		t.Fatal(err)
	}
	if raw != "0.000000000000000000" || payout != "0" {
		t.Fatalf("CalculateTotals() = %s, %s", raw, payout)
	}
}

func TestCalculateTotalsRejectsInvalidAmount(t *testing.T) {
	_, _, err := CalculateTotals([]Record{{ID: 1, Amount: "not-an-amount"}})
	if err == nil {
		t.Fatal("CalculateTotals() error = nil")
	}
}

func TestCalculateTotalsPreservesDECIMALBoundaryCarry(t *testing.T) {
	raw, payout, err := CalculateTotals([]Record{
		{ID: 1, Amount: "99999999999999999999999999999999999999999999999.999999999999999999"},
		{ID: 2, Amount: "0.000000000000000001"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if raw != "100000000000000000000000000000000000000000000000.000000000000000000" ||
		payout != "100000000000000000000000000000000000000000000000" {
		t.Fatalf("CalculateTotals() = %s, %s", raw, payout)
	}
}

func TestCalculateTotalsTrimsPayoutTrailingZeros(t *testing.T) {
	raw, payout, err := CalculateTotals([]Record{{ID: 1, Amount: "1.123456000000000000"}})
	if err != nil {
		t.Fatal(err)
	}
	if raw != "1.123456000000000000" || payout != "1.123456" {
		t.Fatalf("CalculateTotals() = %s, %s", raw, payout)
	}
}
