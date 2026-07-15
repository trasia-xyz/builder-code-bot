package signing

import (
	"strings"
	"testing"
	"time"

	"builder-code-bot/internal/secret"
)

const spotSendTestPrivateKey = "0x822e9959e022b78423eb653a62ea0020cd283e71a2a8133a6ff2aeffaf373cff"

func TestSignSpotSendMatchesOfficialPythonSDKVectors(t *testing.T) {
	// Generated with hyperliquid-python-sdk commit
	// 2fdb18f9517675ea03695a0962bd19eece9c83f0 and eth-account 0.13.5.
	tests := []struct {
		chain string
		want  Signature
	}{
		{chain: "Mainnet", want: Signature{
			R: "0x8c62a0f4d31d07b48b3abf3b522baecd0580650b2993811fc2677f031d0ed709",
			S: "0x7cb40b3b2f236222afe6f83d7bfe5200cfeb53e2f8f67b7dc696ef9e149c8008",
			V: 27,
		}},
		{chain: "Testnet", want: Signature{
			R: "0xab20adb87d454931f60d069620d13a31a17d5a0be2d99b26c7096ab1783213b4",
			S: "0x154423ba62674549cbd857173173b204a18b55e855f0621fa56e2743516b5270",
			V: 27,
		}},
	}

	for _, test := range tests {
		t.Run(test.chain, func(t *testing.T) {
			action := testSpotSendAction()
			action.HyperliquidChain = test.chain
			got, err := SignSpotSend(testSpotSendPrivateKey(t), action)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("SignSpotSend() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestSignSpotSendRecoversSigner(t *testing.T) {
	action := testSpotSendAction()
	key := testSpotSendPrivateKey(t)
	sig, err := SignSpotSend(key, action)
	if err != nil {
		t.Fatal(err)
	}
	address, err := key.Address()
	if err != nil {
		t.Fatal(err)
	}
	got, err := RecoverSpotSendSigner(action, sig)
	if err != nil || got != strings.ToLower(address) {
		t.Fatalf("RecoverSpotSendSigner() = %q, %v", got, err)
	}
}

func TestSpotSendSupportsOfficialChainLabels(t *testing.T) {
	for _, chain := range []string{"Mainnet", "Testnet"} {
		t.Run(chain, func(t *testing.T) {
			action := testSpotSendAction()
			action.HyperliquidChain = chain
			key := testSpotSendPrivateKey(t)
			sig, err := SignSpotSend(key, action)
			if err != nil {
				t.Fatal(err)
			}
			want, err := key.Address()
			if err != nil {
				t.Fatal(err)
			}
			got, err := RecoverSpotSendSigner(action, sig)
			if err != nil || got != strings.ToLower(want) {
				t.Fatalf("RecoverSpotSendSigner() = %q, %v", got, err)
			}
		})
	}
}

func TestSpotSendUsesFixedOfficialSchema(t *testing.T) {
	if SpotSendPrimaryType != "HyperliquidTransaction:SpotSend" {
		t.Fatalf("SpotSendPrimaryType = %q", SpotSendPrimaryType)
	}
	if UserSignDomainName != "HyperliquidSignTransaction" || UserSignDomainVersion != "1" {
		t.Fatalf("user signing domain = %q version %q", UserSignDomainName, UserSignDomainVersion)
	}
	if DefaultSignatureChainID != "0x66eee" {
		t.Fatalf("DefaultSignatureChainID = %q", DefaultSignatureChainID)
	}
	const wantType = "HyperliquidTransaction:SpotSend(string hyperliquidChain,string destination,string token,string amount,uint64 time)"
	if spotSendType != wantType {
		t.Fatalf("spotSendType = %q, want %q", spotSendType, wantType)
	}
}

func TestSpotSendFieldChangesInvalidateSignature(t *testing.T) {
	action := testSpotSendAction()
	sig, err := SignSpotSend(testSpotSendPrivateKey(t), action)
	if err != nil {
		t.Fatal(err)
	}

	mutations := map[string]func(*SpotSendAction){
		"chain":       func(a *SpotSendAction) { a.HyperliquidChain = "Testnet" },
		"destination": func(a *SpotSendAction) { a.Destination = "0x0000000000000000000000000000000000000002" },
		"token":       func(a *SpotSendAction) { a.Token = "USDC:0x00000000000000000000000000000000" },
		"amount":      func(a *SpotSendAction) { a.Amount = "1.000002" },
		"time":        func(a *SpotSendAction) { a.Time++ },
	}
	original, err := RecoverSpotSendSigner(action, sig)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := action
			mutate(&changed)
			got, err := RecoverSpotSendSigner(changed, sig)
			if err == nil && got == original {
				t.Fatalf("changed %s retained signer %s", name, got)
			}
		})
	}
}

func TestNonceGeneratorUsesTimeAndIncrementsWithinMillisecond(t *testing.T) {
	fixed := time.UnixMilli(1750000000000)
	gen := newNonceGenerator(func() time.Time { return fixed })
	if got := gen.Next(); got != 1750000000000 {
		t.Fatalf("first nonce = %d", got)
	}
	if got := gen.Next(); got != 1750000000001 {
		t.Fatalf("second nonce = %d", got)
	}
	if got := gen.Next(); got != 1750000000002 {
		t.Fatalf("third nonce = %d", got)
	}
}

func testSpotSendAction() SpotSendAction {
	return SpotSendAction{
		Type:             "spotSend",
		HyperliquidChain: "Mainnet",
		SignatureChainID: DefaultSignatureChainID,
		Destination:      "0x0000000000000000000000000000000000000001",
		Token:            "USDC:0x6d1e7cde53ba9467b783cb7c530ce054",
		Amount:           "1.000001",
		Time:             1750000000000,
	}
}

func testSpotSendPrivateKey(t *testing.T) PrivateKey {
	t.Helper()
	key, err := ParsePrivateKey(secret.NewString(spotSendTestPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	return key
}
