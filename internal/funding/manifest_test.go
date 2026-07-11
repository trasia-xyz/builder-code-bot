package funding

import (
	"testing"

	"hyperliquid-builder-code-bot/internal/hyperliquid/info"
)

func TestBuildManifestNormalizesRecordOrderAndHash(t *testing.T) {
	token := testToken()
	input := ManifestInput{
		Records: []Record{
			{ID: 3, PeriodStartAt: 20, Amount: "0.123456000000000000"},
			{ID: 2, PeriodStartAt: 10, Amount: "1.000000000000000001"},
			{ID: 1, PeriodStartAt: 10, Amount: "0.000000000000000000"},
		},
		Token:      &token,
		Builders:   []string{"0xBuilderA", "0xBuilderB"},
		Settlement: "0xSettlement",
		Recipient:  "0xRecipient",
	}

	manifest, err := BuildManifest(input)
	if err != nil {
		t.Fatal(err)
	}
	if got := []uint64{manifest.Records[0].ID, manifest.Records[1].ID, manifest.Records[2].ID}; got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("record IDs = %v", got)
	}
	if manifest.RawTotal != "1.123456000000000001" || manifest.PayoutTotal != "1.123457" {
		t.Fatalf("totals = %s, %s", manifest.RawTotal, manifest.PayoutTotal)
	}
	hash, err := HashManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ManifestHash == "" || manifest.ManifestHash != hash {
		t.Fatalf("manifest hash = %q, recomputed = %q", manifest.ManifestHash, hash)
	}

	sortedInput := input
	sortedInput.Records = append([]Record(nil), manifest.Records...)
	sorted, err := BuildManifest(sortedInput)
	if err != nil {
		t.Fatal(err)
	}
	if sorted.ManifestHash != manifest.ManifestHash {
		t.Fatalf("hashes differ by input order: %s != %s", sorted.ManifestHash, manifest.ManifestHash)
	}
}

func TestBuildManifestHashChangesWithRecordIdentityOrAmount(t *testing.T) {
	token := testToken()
	input := ManifestInput{
		Records:    []Record{{ID: 1, PeriodStartAt: 10, Amount: "1.000000000000000000"}},
		Token:      &token,
		Builders:   []string{"0xBuilder"},
		Settlement: "0xSettlement",
		Recipient:  "0xRecipient",
	}
	base, err := BuildManifest(input)
	if err != nil {
		t.Fatal(err)
	}

	for _, change := range []struct {
		name   string
		modify func(*ManifestInput)
	}{
		{name: "ID", modify: func(in *ManifestInput) { in.Records[0].ID++ }},
		{name: "amount", modify: func(in *ManifestInput) { in.Records[0].Amount = "1.000000000000000001" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := input
			changed.Records = append([]Record(nil), input.Records...)
			change.modify(&changed)
			manifest, buildErr := BuildManifest(changed)
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			if manifest.ManifestHash == base.ManifestHash {
				t.Fatalf("hash did not change: %s", manifest.ManifestHash)
			}
		})
	}
}

func TestHashManifestCoversEveryIdentityField(t *testing.T) {
	for _, change := range []struct {
		name   string
		modify func(*Manifest)
	}{
		{name: "raw total", modify: func(m *Manifest) { m.RawTotal = "2.000000000000000000" }},
		{name: "payout total", modify: func(m *Manifest) { m.PayoutTotal = "2" }},
		{name: "token", modify: func(m *Manifest) { m.Token.MetaHash = "changed" }},
		{name: "builders", modify: func(m *Manifest) { m.Builders[0] = "0xChanged" }},
		{name: "settlement", modify: func(m *Manifest) { m.Settlement = "0xChanged" }},
		{name: "recipient", modify: func(m *Manifest) { m.Recipient = "0xChanged" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			token := testToken()
			manifest, err := BuildManifest(ManifestInput{
				Records: []Record{{ID: 1, Amount: "1.000000000000000000"}},
				Token:   &token, Builders: []string{"0xBuilder"},
				Settlement: "0xSettlement", Recipient: "0xRecipient",
			})
			if err != nil {
				t.Fatal(err)
			}
			originalHash := manifest.ManifestHash
			change.modify(&manifest)
			changedHash, err := HashManifest(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if changedHash == originalHash {
				t.Fatalf("hash did not change: %s", changedHash)
			}
		})
	}
}

func TestBuildManifestCopiesInput(t *testing.T) {
	token := testToken()
	records := []Record{{ID: 1, PeriodStartAt: 10, Amount: "1.000000000000000000"}}
	builders := []string{"0xBuilder"}
	manifest, err := BuildManifest(ManifestInput{
		Records: records, Token: &token, Builders: builders,
		Settlement: "0xSettlement", Recipient: "0xRecipient",
	})
	if err != nil {
		t.Fatal(err)
	}

	records[0].Amount = "9.000000000000000000"
	builders[0] = "0xChanged"
	token.Name = "CHANGED"
	if manifest.Records[0].Amount != "1.000000000000000000" ||
		manifest.Builders[0] != "0xBuilder" || manifest.Token.Name != "USDC" {
		t.Fatalf("manifest changed through input aliases: %+v", manifest)
	}
}

func TestBuildManifestAllowsNilTokenForZeroTotal(t *testing.T) {
	manifest, err := BuildManifest(ManifestInput{
		Records:    []Record{{ID: 1, Amount: "0.000000000000000000"}},
		Builders:   []string{"0xBuilder"},
		Settlement: "0xSettlement",
		Recipient:  "0xRecipient",
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Token != nil || manifest.PayoutTotal != "0" {
		t.Fatalf("zero-total manifest = %+v", manifest)
	}
}

func TestBuildManifestRequiresTokenForPositiveTotal(t *testing.T) {
	_, err := BuildManifest(ManifestInput{
		Records:    []Record{{ID: 1, Amount: "1.000000000000000000"}},
		Settlement: "0xSettlement",
		Recipient:  "0xRecipient",
	})
	if err == nil {
		t.Fatal("BuildManifest() error = nil")
	}
}

func TestHashManifestRejectsNonCanonicalRecordOrder(t *testing.T) {
	_, err := HashManifest(Manifest{
		Records: []Record{
			{ID: 2, PeriodStartAt: 10, Amount: "1.000000000000000000"},
			{ID: 1, PeriodStartAt: 10, Amount: "1.000000000000000000"},
		},
	})
	if err == nil {
		t.Fatal("HashManifest() error = nil")
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
	if len(first) != 32 || len(second) != 32 || first == second {
		t.Fatalf("NewRunID() = %q, %q", first, second)
	}
}

func testToken() info.Token {
	return info.Token{
		Name: "USDC", TokenID: "0x01", Index: 0, WeiDecimals: 6,
		WireToken: "USDC:0x01", MetaHash: "metadata-hash",
	}
}
