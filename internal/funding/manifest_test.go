package funding

import (
	"testing"

	"hyperliquid-builder-code-bot/internal/hyperliquid/info"
)

func TestBuildManifestNormalizesRecordOrderAndTotals(t *testing.T) {
	token := testToken()
	manifest, err := BuildManifest(ManifestInput{
		Records: []Record{
			{ID: 3, PeriodStartAt: 20, Amount: "0.123456000000000000"},
			{ID: 2, PeriodStartAt: 10, Amount: "1.000000000000000001"},
			{ID: 1, PeriodStartAt: 10, Amount: "0.000000000000000000"},
		},
		Token: &token, Settlement: "0xSettlement", Recipient: "0xRecipient",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := []uint64{manifest.Records[0].ID, manifest.Records[1].ID, manifest.Records[2].ID}; got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("record IDs = %v", got)
	}
	if manifest.RawTotal != "1.123456000000000001" || manifest.PayoutTotal != "1.123457" {
		t.Fatalf("totals = %s, %s", manifest.RawTotal, manifest.PayoutTotal)
	}
}

func TestBuildManifestCopiesInputs(t *testing.T) {
	token := testToken()
	records := []Record{{ID: 1, Amount: "1.000000000000000000"}}
	manifest, err := BuildManifest(ManifestInput{Records: records, Token: &token, Settlement: "s", Recipient: "r"})
	if err != nil {
		t.Fatal(err)
	}
	records[0].Amount = "9"
	token.Name = "CHANGED"
	if manifest.Records[0].Amount != "1.000000000000000000" || manifest.Token.Name != "USDC" {
		t.Fatalf("manifest changed through input aliases: %+v", manifest)
	}
}

func TestBuildManifestAllowsNilTokenOnlyForZeroTotal(t *testing.T) {
	manifest, err := BuildManifest(ManifestInput{
		Records: []Record{{ID: 1, Amount: "0.000000000000000000"}}, Settlement: "s", Recipient: "r",
	})
	if err != nil || manifest.Token != nil || manifest.PayoutTotal != "0" {
		t.Fatalf("zero manifest = %+v, %v", manifest, err)
	}
	if _, err := BuildManifest(ManifestInput{
		Records: []Record{{ID: 1, Amount: "1.000000000000000000"}}, Settlement: "s", Recipient: "r",
	}); err == nil {
		t.Fatal("positive manifest accepted without token")
	}
}

func TestNewRunIDReturnsDistinctIDs(t *testing.T) {
	first, err := NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 16 || len(second) != 16 || first == second {
		t.Fatalf("NewRunID() = %q, %q", first, second)
	}
}

func testToken() info.Token {
	return info.Token{Name: "USDC", TokenID: "0x01", Index: 0, WeiDecimals: 6, WireToken: "USDC:0x01"}
}
