package signing

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"

	"builder-code-bot/internal/hyperliquid"
)

type Signature struct {
	R string `json:"r"`
	S string `json:"s"`
	V int    `json:"v"`
}

type L1ActionSignInput struct {
	Action       any
	Nonce        uint64
	Network      hyperliquid.Network
	VaultAddress *string
	ExpiresAfter *uint64
}

type L1ActionRecoverInput struct {
	Action       any
	Nonce        uint64
	Network      hyperliquid.Network
	VaultAddress *string
	ExpiresAfter *uint64
	Signature    Signature
}

func SignL1Action(key PrivateKey, input L1ActionSignInput) (Signature, error) {
	privateKey, err := key.require()
	if err != nil {
		return Signature{}, err
	}
	connectionID, err := ActionHash(ActionHashInput{Action: input.Action, Nonce: input.Nonce, VaultAddress: input.VaultAddress, ExpiresAfter: input.ExpiresAfter})
	if err != nil {
		return Signature{}, err
	}
	digest, err := l1AgentDigest(connectionID, input.Network)
	if err != nil {
		return Signature{}, err
	}
	return signDigest(privateKey, digest)
}

func RecoverL1ActionSigner(input L1ActionRecoverInput) (string, error) {
	connectionID, err := ActionHash(ActionHashInput{Action: input.Action, Nonce: input.Nonce, VaultAddress: input.VaultAddress, ExpiresAfter: input.ExpiresAfter})
	if err != nil {
		return "", err
	}
	digest, err := l1AgentDigest(connectionID, input.Network)
	if err != nil {
		return "", err
	}
	return recoverDigestSigner(digest, input.Signature)
}

func signDigest(privateKey *secp256k1.PrivateKey, digest [32]byte) (Signature, error) {
	signed := ecdsa.SignCompact(privateKey, digest[:], false)
	if len(signed) != 65 {
		return Signature{}, fmt.Errorf("unexpected signature length %d", len(signed))
	}
	return Signature{
		R: "0x" + hex.EncodeToString(signed[1:33]),
		S: "0x" + hex.EncodeToString(signed[33:65]),
		V: int(signed[0]),
	}, nil
}

func recoverDigestSigner(digest [32]byte, signature Signature) (string, error) {
	compact, err := compactSignature(signature)
	if err != nil {
		return "", err
	}
	publicKey, _, err := ecdsa.RecoverCompact(compact, digest[:])
	if err != nil {
		return "", fmt.Errorf("recover compact signature: %w", err)
	}
	serialized := publicKey.SerializeUncompressed()
	hash := keccak256(serialized[1:])
	return strings.ToLower(checksumAddress(hash[12:])), nil
}

func compactSignature(signature Signature) ([]byte, error) {
	if signature.V < 27 || signature.V > 34 {
		return nil, fmt.Errorf("signature.v must be in compact recovery range 27..34")
	}
	r, err := fixedHexBytes("signature.r", signature.R, 32)
	if err != nil {
		return nil, err
	}
	s, err := fixedHexBytes("signature.s", signature.S, 32)
	if err != nil {
		return nil, err
	}
	return append([]byte{byte(signature.V)}, append(r, s...)...), nil
}

func fixedHexBytes(field, value string, length int) ([]byte, error) {
	value = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(value), "0x"), "0X")
	if len(value) != length*2 {
		return nil, fmt.Errorf("%s must be %d bytes", field, length)
	}
	out, err := hex.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", field, err)
	}
	return out, nil
}
