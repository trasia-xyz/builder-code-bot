package signing_test

import (
	"strings"
	"testing"

	"builder-code-bot/internal/hyperliquid"
	"builder-code-bot/internal/hyperliquid/signing"
	"builder-code-bot/internal/secret"
)

const (
	knownVectorPrivateKey   = "0x822e9959e022b78423eb653a62ea0020cd283e71a2a8133a6ff2aeffaf373cff"
	knownVectorNonce        = uint64(1234567890)
	knownVectorVaultAddress = "0x1234567890123456789012345678901234567890"
	knownVectorExpiresAfter = uint64(1234567890)
)

var knownVectorL1Action = signing.Object{
	signing.F("type", "order"),
	signing.F("orders", []any{
		signing.Object{
			signing.F("a", 0),
			signing.F("b", true),
			signing.F("p", "30000"),
			signing.F("s", "0.1"),
			signing.F("r", false),
			signing.F("t", signing.Object{
				signing.F("limit", signing.Object{
					signing.F("tif", "Gtc"),
				}),
			}),
		},
	}),
	signing.F("grouping", "na"),
}

func TestActionHashMatchesKnownVectors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		vaultAddress *string
		expiresAfter *uint64
		want         string
	}{
		{name: "base", want: "0x25367e0dba84351148288c2233cd6130ed6cec5967ded0c0b7334f36f957cc90"},
		{name: "with vault", vaultAddress: ptr(knownVectorVaultAddress), want: "0x214e2ea3270981b6fd18174216691e69f56872663139d396b10ded319cb4bb1e"},
		{name: "with expires", expiresAfter: ptr(knownVectorExpiresAfter), want: "0xc30b002ba3775e4c31c43c1dfd3291dfc85c6ae06c6b9f393991de86cad5fac7"},
		{name: "with vault and expires", vaultAddress: ptr(knownVectorVaultAddress), expiresAfter: ptr(knownVectorExpiresAfter), want: "0x2d62412aa0fc57441b5189841d81554a6a9680bf07204e1454983a9ca44f0744"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := signing.ActionHashHex(signing.ActionHashInput{
				Action: knownVectorL1Action, Nonce: knownVectorNonce,
				VaultAddress: tt.vaultAddress, ExpiresAfter: tt.expiresAfter,
			})
			if err != nil {
				t.Fatalf("ActionHashHex() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ActionHashHex() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestActionHashRejectsNonWireModelInput(t *testing.T) {
	_, err := signing.ActionHash(signing.ActionHashInput{Action: map[string]any{"type": "claimRewards"}, Nonce: 1})
	if err == nil || !strings.Contains(err.Error(), "signing.Object") {
		t.Fatalf("ActionHash() error = %v", err)
	}
}

func TestSignL1ActionMatchesKnownVectors(t *testing.T) {
	t.Parallel()

	privateKey, err := signing.ParsePrivateKey(secret.NewString(knownVectorPrivateKey))
	if err != nil {
		t.Fatalf("ParsePrivateKey() error = %v", err)
	}
	tests := []struct {
		name    string
		network hyperliquid.Network
		want    signing.Signature
	}{
		{name: "mainnet", network: hyperliquid.NetworkMainnet, want: signing.Signature{
			R: "0x61078d8ffa3cb591de045438a1ae2ed299b271891d1943a33901e7cfb3a31ed8",
			S: "0x0e91df4f9841641d3322dad8d932874b74d7e082cdb5b533f804964a6963aef9", V: 28,
		}},
		{name: "testnet", network: hyperliquid.NetworkTestnet, want: signing.Signature{
			R: "0x6b0283a894d87b996ad0182b86251cc80d27d61ef307449a2ed249a508ded1f7",
			S: "0x6f884e79f4a0a10af62db831af6f8e03b3f11d899eb49b352f836746ee9226da", V: 27,
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := signing.SignL1Action(privateKey, signing.L1ActionSignInput{
				Action: knownVectorL1Action, Nonce: knownVectorNonce, Network: tt.network,
			})
			if err != nil {
				t.Fatalf("SignL1Action() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("SignL1Action() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRecoverL1ActionSigner(t *testing.T) {
	privateKey, err := signing.ParsePrivateKey(secret.NewString(knownVectorPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	wantAddress, err := privateKey.Address()
	if err != nil {
		t.Fatal(err)
	}
	signature, err := signing.SignL1Action(privateKey, signing.L1ActionSignInput{
		Action: knownVectorL1Action, Nonce: knownVectorNonce, Network: hyperliquid.NetworkTestnet,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := signing.RecoverL1ActionSigner(signing.L1ActionRecoverInput{
		Action: knownVectorL1Action, Nonce: knownVectorNonce,
		Network: hyperliquid.NetworkTestnet, Signature: signature,
	})
	if err != nil || got != strings.ToLower(wantAddress) {
		t.Fatalf("RecoverL1ActionSigner() = %q, %v", got, err)
	}
}

func TestActionHashRejectsInvalidVaultAddress(t *testing.T) {
	_, err := signing.ActionHash(signing.ActionHashInput{
		Action: knownVectorL1Action, Nonce: knownVectorNonce, VaultAddress: ptr("0x1234"),
	})
	if err == nil {
		t.Fatal("ActionHash() error = nil, want error")
	}
}

func ptr[T any](value T) *T { return &value }
